package agentguard

import (
	"context"
	"time"
)

// Replicas and verification (§6.10 steps 6 and 8).

// verifyPoll is the pause between verification rounds.
const verifyPoll = 100 * time.Millisecond

// containReplicas ends agent sessions on each configured replica of the
// cluster's databases and names the standbys nobody configured.
func (r *clusterRun) containReplicas(ctx context.Context) {
	seen := map[string]bool{}
	for _, t := range r.targets {
		for _, rep := range t.Replicas {
			if seen[rep.Name+"\x00"+rep.DSN] {
				continue
			}
			seen[rep.Name+"\x00"+rep.DSN] = true
			run := r.s.openReplica(ctx, t.Name, rep)
			run.contain(ctx, r.k, r.scopeDatname())
			r.replicas = append(r.replicas, run)
		}
	}
	r.observeStandbys(ctx)
}

// observeStandbys reads pg_stat_replication on the primary and on each
// configured replica (cascading standbys) and keeps the unconfigured ones.
func (r *clusterRun) observeStandbys(ctx context.Context) {
	configured := map[string]bool{}
	obs, err := observeStandbys(ctx, r.targets[0].Pool, "primary")
	r.dbs[0].Error = joinErr(r.dbs[0].Error, err)
	for _, rr := range r.replicas {
		configured[rr.replica.Name] = true
		if rr.cluster != "" {
			configured[rr.cluster] = true
		}
		if rr.conn == nil {
			continue
		}
		more, err := observeStandbys(ctx, rr.conn, rr.replica.Name)
		rr.report.Error = joinErr(rr.report.Error, err)
		obs = append(obs, more...)
	}
	r.standbys = classifyStandbys(obs, configured, SessionBoundFor(r.s.cfg.Roles, r.version))
}

// check is one verification round. clean: nothing of the scope is left;
// settled: nothing more can change (clean, or what is left is a replica
// pg_sage cannot reach), so verification stops waiting for it.
func (r *clusterRun) check(ctx context.Context) (clean, settled bool) {
	clean = r.checkPrimary(ctx)
	settled = clean
	for _, rr := range r.replicas {
		if rr.check(ctx, r.k, r.scopeDatname()) {
			continue
		}
		clean = false
		if rr.conn != nil {
			settled = false
		}
	}
	return clean, settled
}

// checkPrimary ends any agent backend still there (one that raced the
// kill) and re-disables any role a concurrent change re-enabled.
func (r *clusterRun) checkPrimary(ctx context.Context) bool {
	primary := r.targets[0].Pool
	n, err := countAgents(ctx, primary, r.k, r.scopeDatname())
	if err != nil {
		return false
	}
	clean := true
	if n > 0 {
		clean = false
		more, _ := terminate(ctx, primary, r.k, r.scopeDatname())
		r.dbs[0].BackendsTerminated += more
	}
	if !r.k.disablesRoles() {
		return clean
	}
	blocked, err := loginsBlocked(ctx, primary, r.k)
	if err != nil {
		return false
	}
	if !blocked {
		r.redisable(ctx)
		return false
	}
	return clean
}

// redisable disables the roles in scope that can log in again.
func (r *clusterRun) redisable(ctx context.Context) {
	rows, err := readRoleAttrs(ctx, r.targets[0].Pool, r.k.roles(), r.k.all)
	if err != nil {
		return
	}
	for _, row := range rows {
		if !row.attrs.killed() {
			stmt, _ := r.s.disableRole(ctx, r.targets[0].Pool, row.name)
			r.statements = append(r.statements, stmt)
		}
	}
}

// verify polls every cluster until it is clean or the verification
// timeout passes, then completes each database's report.
func (s *Switch) verify(ctx context.Context, runs []*clusterRun) {
	deadline := time.Now().Add(s.cfg.VerifyTimeout)
	clean := make([]bool, len(runs))
	settled := make([]bool, len(runs))
	for {
		all := true
		for i, r := range runs {
			if !settled[i] {
				clean[i], settled[i] = r.check(ctx)
			}
			all = all && settled[i]
		}
		if all || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			deadline = time.Now()
		case <-time.After(verifyPoll):
		}
	}
	for i, r := range runs {
		r.finish(clean[i])
	}
}

// finish attaches replica reports and sets Verified per database.
func (r *clusterRun) finish(clean bool) {
	for i := range r.dbs {
		ok := clean && r.dbs[i].Error == ""
		for _, rr := range r.replicas {
			if rr.target != r.targets[i].Name {
				continue
			}
			rr.close()
			r.dbs[i].Replicas = append(r.dbs[i].Replicas, rr.report)
			ok = ok && rr.report.Verified
		}
		r.dbs[i].Verified = ok
	}
	r.dbs[0].Replicas = append(r.dbs[0].Replicas, r.standbys...)
}
