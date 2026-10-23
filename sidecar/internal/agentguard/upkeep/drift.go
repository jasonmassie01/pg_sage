package upkeep

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// JobDriftCorrection names the narrowing corrections of the drift
// reconciler in the audit.
const JobDriftCorrection = "drift_correction"

// DriftFindingCategory is an agent role whose catalog state differs from
// what governance recorded (§6.6), keyed by role name: critical when the
// drift widens access, a warning when it only narrows it.
const DriftFindingCategory = "agent_role_drift"

// RoleDrift is one agent role's drift in one database. Attribute and
// membership drift is cluster-wide and reported once, in the cluster's
// administering database.
type RoleDrift struct {
	PrincipalID string   `json:"principal_id"`
	Role        string   `json:"role"`
	ClusterKey  string   `json:"cluster_key"`
	Database    string   `json:"database"`
	Widening    []string `json:"widening,omitempty"`
	Narrowing   []string `json:"narrowing,omitempty"`
	// Corrected are the narrowing statements governance ran (guard_revoke).
	Corrected []string `json:"corrected,omitempty"`
	ActionIDs []int64  `json:"action_ids,omitempty"`
	// Fix is what a person runs for drift governance does not own.
	Fix string `json:"fix,omitempty"`
}

func (d RoleDrift) empty() bool { return len(d.Widening) == 0 && len(d.Narrowing) == 0 }

// DriftReport is one reconcile pass. Failed is keyed by cluster key,
// database or "database/role".
type DriftReport struct {
	Clusters int               `json:"clusters"`
	Roles    int               `json:"roles"`
	Drift    []RoleDrift       `json:"drift"`
	Failed   map[string]string `json:"failed,omitempty"`
}

// ReconcileDrift compares every registered agent role of every cluster
// with what governance recorded: its attributes and memberships
// (ReadRoleState, AgentViolations, the login and connection limit its
// status implies) and, in each database, its effective privileges against
// its registry grants plus the PUBLIC baseline. Narrowing corrections
// governance owns run as guard_revoke (an excess privilege pg_sage
// granted, a login or connection limit wider than recorded); the rest is
// reported with the statement a person runs, never forced.
func (r *Runner) ReconcileDrift(ctx context.Context, f Fence) (DriftReport, error) {
	rep := DriftReport{Failed: map[string]string{}}
	if err := f.check(ctx, r.control); err != nil {
		return rep, err
	}
	targets, err := r.targets(ctx)
	if err != nil {
		return rep, fmt.Errorf("upkeep: listing databases for drift: %w", err)
	}
	groups, noKey := groupClusters(targets)
	for _, t := range noKey {
		rep.Failed[t.Name] = "the cluster identity of this database is unknown"
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rep.Clusters++
		if err := r.driftCluster(ctx, f, key, groups[key], &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// clusterPass is one cluster's drift pass.
type clusterPass struct {
	r     *Runner
	f     Fence
	key   string
	dbs   []agentguard.KillTarget
	rep   *DriftReport
	drift map[string][]RoleDrift // database -> drifts
	keep  map[string][]string    // database -> roles whose finding stays as is
}

func (r *Runner) driftCluster(ctx context.Context, f Fence, key string,
	dbs []agentguard.KillTarget, rep *DriftReport) error {
	roles, err := r.store.ClusterRolesOn(ctx, key)
	if err != nil {
		rep.Failed[key] = err.Error()
		return nil
	}
	p := &clusterPass{r: r, f: f, key: key, dbs: dbs, rep: rep,
		drift: map[string][]RoleDrift{}, keep: map[string][]string{}}
	for _, cr := range roles {
		if cr.Status == agentguard.RoleStatusRetired {
			continue
		}
		for _, role := range []string{cr.BrokerRole, cr.LoginRole} {
			rep.Roles++
			if err := p.role(ctx, cr, role); err != nil {
				return err
			}
		}
	}
	return p.findings(ctx)
}

// role checks one role: its cluster-wide state once, then each database.
func (p *clusterPass) role(ctx context.Context, cr agentguard.ClusterRole,
	role string) error {
	admin := p.dbs[0]
	st, err := agentguard.ReadRoleState(ctx, admin.Pool, role)
	if errors.Is(err, agentguard.ErrNotFound) {
		p.add(admin.Name, RoleDrift{Narrowing: []string{"role " + role + " is registered " +
			"but missing from the cluster"}}, cr, role)
		return nil
	}
	if err != nil {
		p.fail(admin.Name, role, err)
		return nil
	}
	d, fixes := attributeDrift(st, expectedAttrs(cr, role, p.r.cfg.Roles))
	if err := p.correct(ctx, admin, cr, role, &d, fixes); err != nil {
		return err
	}
	p.add(admin.Name, d, cr, role)
	for _, db := range p.dbs {
		pd, err := p.privileges(ctx, db, cr, role)
		if err != nil {
			if errors.Is(err, ErrFenced) {
				return err
			}
			p.fail(db.Name, role, err)
			continue
		}
		p.add(db.Name, pd, cr, role)
	}
	return nil
}

func (p *clusterPass) fail(db, role string, err error) {
	p.rep.Failed[db+"/"+role] = err.Error()
	p.keep[db] = append(p.keep[db], role)
}

// add records d for role in db, merging with what is already there.
func (p *clusterPass) add(db string, d RoleDrift, cr agentguard.ClusterRole, role string) {
	if d.empty() {
		return
	}
	d.PrincipalID, d.Role, d.ClusterKey, d.Database = cr.PrincipalID, role, p.key, db
	for i, have := range p.drift[db] {
		if have.Role == role {
			p.drift[db][i] = mergeDrift(have, d)
			return
		}
	}
	p.drift[db] = append(p.drift[db], d)
}

func mergeDrift(a, b RoleDrift) RoleDrift {
	a.Widening = append(a.Widening, b.Widening...)
	a.Narrowing = append(a.Narrowing, b.Narrowing...)
	a.Corrected = append(a.Corrected, b.Corrected...)
	a.ActionIDs = append(a.ActionIDs, b.ActionIDs...)
	a.Fix = strings.TrimSpace(strings.Join([]string{a.Fix, b.Fix}, "\n"))
	return a
}

// findings keeps one drift finding per role in each database in step and
// adds the drift to the report.
func (p *clusterPass) findings(ctx context.Context) error {
	for _, db := range p.dbs {
		if err := p.f.check(ctx, p.r.control); err != nil {
			return err
		}
		keep := append([]string{}, p.keep[db.Name]...)
		for _, d := range p.drift[db.Name] {
			p.rep.Drift = append(p.rep.Drift, d)
			keep = append(keep, d.Role)
			if err := raise(ctx, db.Pool, driftFinding(d)); err != nil {
				p.rep.Failed[db.Name] = err.Error()
			}
		}
		if err := resolveOthers(ctx, db.Pool, DriftFindingCategory, keep); err != nil {
			p.rep.Failed[db.Name] = err.Error()
		}
	}
	return nil
}

func driftFinding(d RoleDrift) finding {
	severity, what := "warning", "has less access than governance recorded"
	if len(d.Widening) > 0 {
		severity, what = "critical", "has more access than governance recorded"
	}
	problems := append(append([]string{}, d.Widening...), d.Narrowing...)
	rec := "Governance revoked what it owns (see the corrected statements) and never " +
		"forces the rest. Find who changed the role outside pg_sage; run the statements " +
		"shown as the grantor named in them."
	if len(d.Widening) == 0 {
		rec = "A grant or role governance recorded is missing. Re-approve the grant if " +
			"the agent still needs it, or revoke it in pg_sage so the registry matches."
	}
	return finding{category: DriftFindingCategory, severity: severity, objectType: "role",
		ident: d.Role, sql: d.Fix, recommendation: rec,
		title: fmt.Sprintf("Agent role %s %s: %s", d.Role, what, strings.Join(problems,
			"; ")),
		detail: d}
}
