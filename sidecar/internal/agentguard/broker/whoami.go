package broker

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
)

const observationNotice = "trust.level is observation: agent requests other than reads " +
	"are recorded as L1 proposals until an operator raises it to advisory"

// WhoAmI describes the calling principal; agentguard.ErrNoPrincipal when
// the caller is not an agent.
func (b *Broker) WhoAmI(ctx context.Context) (WhoAmI, error) {
	id, ok := agentguard.IdentityFromContext(ctx)
	if !ok {
		return WhoAmI{}, agentguard.ErrNoPrincipal
	}
	p := id.Principal
	w := WhoAmI{Principal: PrincipalView{ID: p.ID, Name: p.Name, Profile: p.Profile,
		EnvCeiling: string(p.EnvCeiling), Status: string(p.Status)},
		Tainted: p.Tainted, Frozen: p.Frozen(), Databases: []DatabaseView{}}
	if p.SponsorUserID != nil {
		w.Sponsor = &SponsorView{UserID: *p.SponsorUserID, Active: p.SponsorActive}
	}
	if b.deps.TrustLevel != nil && b.deps.TrustLevel() == "observation" {
		w.Notice = observationNotice
	}
	for _, name := range b.databasesOf(ctx, id) {
		view, ok, err := b.databaseView(ctx, id, name)
		if err != nil {
			return WhoAmI{}, err
		}
		if ok {
			w.Databases = append(w.Databases, view)
		}
	}
	return w, nil
}

func (b *Broker) databasesOf(ctx context.Context, id agentguard.Identity) []string {
	var names []string
	if b.deps.Databases != nil {
		names = b.deps.Databases(ctx)
	}
	out := []string{}
	for _, n := range names {
		if id.MayUseDatabase(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// databaseView describes one database; ok is false when it no longer
// resolves.
func (b *Broker) databaseView(ctx context.Context, id agentguard.Identity, name string) (
	DatabaseView, bool, error) {
	t, err := b.deps.Targets.Target(ctx, name)
	if errors.Is(err, ErrUnknownDatabase) {
		return DatabaseView{}, false, nil
	}
	if err != nil {
		return DatabaseView{}, false, err
	}
	v := b.deps.Decider.Decide(ctx, decide.Request{PrincipalID: id.Principal.ID,
		Tool: "agent_whoami", Kind: agentguard.ToolAgent, Capability: decide.CapRead,
		Database: name, TaskID: id.TaskID})
	env := string(v.Env)
	if env == "" {
		env = string(t.Env)
	}
	view := DatabaseView{Name: name, Env: env, BindingVerified: t.Verified,
		Lanes: []string{}, Levels: map[string]int{"read": 0}}
	if v.Allowed {
		view.Levels["read"] = 3
	} else {
		view.Reason = string(v.Reason)
	}
	if err := b.addLanes(ctx, &view, id.Principal.ID, t, v.Allowed); err != nil {
		return DatabaseView{}, false, err
	}
	grants, err := brokerGrants(ctx, t.Pool, agentguard.BrokerRoleName(id.Principal.ID))
	if err != nil {
		return DatabaseView{}, false, err
	}
	view.Grants = grants
	return view, true, nil
}

// addLanes lists the brokered lane when the principal has an active
// broker login on the cluster and reads are allowed.
func (b *Broker) addLanes(ctx context.Context, view *DatabaseView, pid string, t Target,
	allowed bool) error {
	if b.deps.Roles == nil {
		return nil
	}
	role, err := b.deps.Roles.ClusterRolesOf(ctx, pid, t.ClusterKey)
	if err != nil {
		return fmt.Errorf("%w: reading the agent's roles: %v", ErrUnavailable, err)
	}
	if allowed && role != nil && role.Status == agentguard.RoleStatusActive {
		view.Lanes = append(view.Lanes, "brokered")
	}
	return nil
}

// capabilities maps a privilege to its capability class.
var capabilities = map[string]string{"SELECT": "read", "INSERT": "write_insert",
	"UPDATE": "write_update", "DELETE": "write_delete"}

// brokerGrants reads the broker role's table and column privileges. The
// grants registry (with expiries) supersedes this once it records them.
func brokerGrants(ctx context.Context, pool *pgxpool.Pool, role string) ([]GrantView,
	error) {
	rows, err := pool.Query(ctx, `/* pg_sage agent_whoami v1 */
		WITH r AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)
		SELECT n.nspname || '.' || c.relname, a.privilege_type, NULL::text[]
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
			pg_catalog.aclexplode(c.relacl) a, r
		WHERE c.relacl IS NOT NULL AND a.grantee = r.oid
		UNION ALL
		SELECT n.nspname || '.' || c.relname, a.privilege_type,
			array_agg(at.attname::text ORDER BY at.attnum)
		FROM pg_catalog.pg_attribute at
		JOIN pg_catalog.pg_class c ON c.oid = at.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
			pg_catalog.aclexplode(at.attacl) a, r
		WHERE at.attacl IS NOT NULL AND a.grantee = r.oid
		GROUP BY 1, 2
		ORDER BY 1, 2`, role)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the agent's grants: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	out := []GrantView{}
	for rows.Next() {
		var g GrantView
		var priv string
		if err := rows.Scan(&g.Object, &priv, &g.Columns); err != nil {
			return nil, fmt.Errorf("%w: reading the agent's grants: %v", ErrUnavailable, err)
		}
		if g.Capability = capabilities[priv]; g.Capability != "" {
			out = append(out, g)
		}
	}
	return out, rows.Err()
}
