package upkeep

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// ClusterViolations are the agent backends of one cluster that break
// G1-01, and the databases whose findings carry them.
type ClusterViolations struct {
	ClusterKey string                      `json:"cluster_key"`
	Databases  []string                    `json:"databases"`
	Findings   []agentguard.BackendFinding `json:"findings"`
}

// BackendReport is one backend-check pass. Failed is keyed by cluster
// key, or by database name for a database whose cluster is unknown.
type BackendReport struct {
	Clusters   int                 `json:"clusters"`
	Violations []ClusterViolations `json:"violations"`
	Failed     map[string]string   `json:"failed,omitempty"`
}

// CheckBackends runs G1-01 on every cluster of the fleet (one connection
// per cluster: pg_stat_activity is cluster-wide) and keeps a critical
// finding per violating role open in each of the cluster's databases;
// a role that no longer violates has its finding resolved. A database
// whose cluster key is unknown is reported, never checked against a
// guessed registry (every agent role would look unregistered).
func (r *Runner) CheckBackends(ctx context.Context, f Fence) (BackendReport, error) {
	rep := BackendReport{Failed: map[string]string{}}
	if err := f.check(ctx, r.control); err != nil {
		return rep, err
	}
	targets, err := r.targets(ctx)
	if err != nil {
		return rep, fmt.Errorf("upkeep: listing databases for the backend check: %w", err)
	}
	groups, noKey := groupClusters(targets)
	for _, t := range noKey {
		rep.Failed[t.Name] = "the cluster identity of this database is unknown; its agent " +
			"backends are checked once it is read"
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rep.Clusters++
		if err := r.checkCluster(ctx, f, key, groups[key], &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (r *Runner) checkCluster(ctx context.Context, f Fence, key string,
	dbs []agentguard.KillTarget, rep *BackendReport) error {
	found, err := agentguard.CheckBackends(ctx, dbs[0].Pool, r.store, key)
	if err != nil {
		rep.Failed[key] = err.Error()
		return nil
	}
	if err := f.check(ctx, r.control); err != nil {
		return err
	}
	keep := make([]string, 0, len(found))
	for _, b := range found {
		keep = append(keep, b.Role)
	}
	var names, errs []string
	for _, db := range dbs {
		names = append(names, db.Name)
		if err := syncBackendFindings(ctx, db, key, found, keep); err != nil {
			errs = append(errs, db.Name+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		rep.Failed[key] = strings.Join(errs, "; ")
	}
	if len(found) > 0 {
		rep.Violations = append(rep.Violations, ClusterViolations{ClusterKey: key,
			Databases: names, Findings: found})
	}
	return nil
}

func syncBackendFindings(ctx context.Context, db agentguard.KillTarget, key string,
	found []agentguard.BackendFinding, keep []string) error {
	for _, b := range found {
		if err := raise(ctx, db.Pool, backendFinding(key, b)); err != nil {
			return err
		}
	}
	return resolveOthers(ctx, db.Pool, BackendFindingCategory, keep)
}

func backendFinding(key string, b agentguard.BackendFinding) finding {
	return finding{category: BackendFindingCategory, severity: "critical",
		objectType: "role", ident: b.Role,
		title: fmt.Sprintf("Agent role %s is connected (%d sessions) and %s", b.Role,
			b.Sessions, strings.Join(b.Problems, ", ")),
		recommendation: "An agent role must be registered and active, without superuser, " +
			"BYPASSRLS, CREATEROLE, CREATEDB, REPLICATION or a server-files or " +
			"write-all-data membership, and own nothing. Kill this agent " +
			"(POST /api/v1/agents/kill) or end its sessions, then find who created or " +
			"changed the role.",
		detail: map[string]any{"cluster_key": key, "role": b.Role, "sessions": b.Sessions,
			"problems": b.Problems}}
}
