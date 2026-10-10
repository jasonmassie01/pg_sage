package grants

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard/grants"))
}

var seq atomic.Int64

func uniq(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano()%1e9, seq.Add(1))
}

// fixture is a PostgreSQL 16+ cluster where pg_sage runs as a
// non-superuser CREATEROLE role, an application schema owned by another
// role, a sponsored principal whose roles exist, and an executor whose
// gate the test controls.
type fixture struct {
	super    *pgxpool.Pool
	admin    *pgxpool.Pool
	adminRol string
	owner    string
	dsn      string
	db       string
	schema   string // application schema: table orders
	store    *agentguard.Store
	exec     *executor.Executor
	manager  *Manager
	runtime  atomic.Value // policy.RuntimeState
	p        agentguard.Principal
	target   Target
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	super, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(super.Close)
	require.NoError(t, schema.Bootstrap(ctx, super))
	var version int
	require.NoError(t, super.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&version))
	if version < agentguard.MinRoleServerVersion {
		t.Skip("agent grants need PostgreSQL 16+ (role management is posture-only below)")
	}
	f := &fixture{super: super, dsn: dsn, store: agentguard.NewStore(super)}
	require.NoError(t, super.QueryRow(ctx, "SELECT current_database()").Scan(&f.db))
	f.createAdmin(t)
	f.createApp(t)
	f.runtime.Store(policy.RuntimeState{ExecutorEnabled: true,
		TrustLevel: policy.TrustAdvisory, ExecutionMode: "auto"})
	f.exec = executor.New(f.admin, config.DefaultConfig(), time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	f.exec.WithPolicyGate(policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return f.runtime.Load().(policy.RuntimeState), nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return policy.UnattendedProfile(), nil
		},
	}))
	// §6.2.7: an agent's change holds its principal active until it commits.
	f.exec.WithPrincipalHold(func(ctx context.Context, pid string) (func(), error) {
		return decide.HoldActive(ctx, super, pid)
	})
	f.manager, err = NewManager(f.store, Config{MaxDuration: 4 * time.Hour})
	require.NoError(t, err)
	f.p = f.principal(t, agentguard.EnvProd, true)
	f.target = Target{Name: f.db, ID: newUUID(t, super), Pool: f.admin, Env: envbind.EnvProd,
		Executor: f.exec}
	return f
}

func newUUID(t *testing.T, q Querier) string {
	t.Helper()
	var id string
	require.NoError(t, q.QueryRow(context.Background(),
		"SELECT md5(random()::text)::uuid::text").Scan(&id))
	return id
}

func (f *fixture) exec1(t *testing.T, sql string) {
	t.Helper()
	_, err := f.super.Exec(context.Background(), sql)
	require.NoError(t, err, sql)
}

// createAdmin makes pg_sage's role: CREATEROLE, CONNECT with grant option,
// and write access to the sage schema.
func (f *fixture) createAdmin(t *testing.T) {
	t.Helper()
	f.adminRol = uniq("g1g_admin")
	for _, s := range []string{
		"CREATE ROLE " + f.adminRol + " LOGIN CREATEROLE PASSWORD 'admin-pw'",
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{f.db}.Sanitize() + " TO " +
			f.adminRol + " WITH GRANT OPTION",
		"GRANT USAGE ON SCHEMA sage TO " + f.adminRol,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA sage TO " + f.adminRol,
		"GRANT USAGE ON ALL SEQUENCES IN SCHEMA sage TO " + f.adminRol,
	} {
		f.exec1(t, s)
	}
	u, err := url.Parse(f.dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(f.adminRol, "admin-pw")
	f.admin, err = pgxpool.New(context.Background(), u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		f.admin.Close()
		f.dropRole(f.adminRol)
	})
}

// createApp makes schema <s> owned by an owner role with table orders.
// pg_sage holds SELECT WITH GRANT OPTION on id, email, secret_token, note
// and region, and plain SELECT on amount (no grant option). Classes: id
// and amount clean, email pii, secret_token secret, region proposed pii
// (pending: proposals only narrow),
// note unclassified.
func (f *fixture) createApp(t *testing.T) {
	t.Helper()
	f.owner, f.schema = uniq("g1g_owner"), uniq("g1g_app")
	s := pgx.Identifier{f.schema}.Sanitize()
	for _, stmt := range []string{
		"CREATE ROLE " + f.owner + " NOLOGIN",
		"CREATE SCHEMA " + s + " AUTHORIZATION " + f.owner,
		"CREATE TABLE " + s + ".orders (id int PRIMARY KEY, email text, " +
			"secret_token text, note text, region text, amount numeric)",
		"ALTER TABLE " + s + ".orders OWNER TO " + f.owner,
		"GRANT USAGE ON SCHEMA " + s + " TO " + f.adminRol + " WITH GRANT OPTION",
		"GRANT SELECT (id, email, secret_token, note, region) ON " + s + ".orders TO " +
			f.adminRol + " WITH GRANT OPTION",
		"GRANT SELECT (amount) ON " + s + ".orders TO " + f.adminRol,
	} {
		f.exec1(t, stmt)
	}
	t.Cleanup(func() {
		_, _ = f.super.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+s+" CASCADE")
		f.dropRole(f.owner)
	})
	cs := classify.NewStore(f.super)
	for col, class := range map[string]classify.Class{"id": classify.ClassClean,
		"amount": classify.ClassClean, "email": classify.ClassPII,
		"secret_token": classify.ClassSecret} {
		c, err := cs.ResolveColumn(context.Background(), f.schema, "orders", col)
		require.NoError(t, err)
		_, err = cs.Set(context.Background(), c, class, "test@example.com", "fixture")
		require.NoError(t, err)
	}
	c, err := cs.ResolveColumn(context.Background(), f.schema, "orders", "region")
	require.NoError(t, err)
	_, _, err = cs.Propose(context.Background(), classify.Proposal{Column: c,
		Class: classify.ClassPII, Source: classify.SourceDetector,
		ProposedBy: classify.RulesProposer, Rationale: "fixture",
		Evidence: []classify.Citation{{Kind: "name", Ref: "region"}}})
	require.NoError(t, err)
}

func (f *fixture) dropRole(name string) {
	ctx := context.Background()
	_, _ = f.super.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
		"WHERE usename = $1", name)
	var exists bool
	_ = f.super.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)",
		name).Scan(&exists)
	if !exists {
		return
	}
	_, _ = f.super.Exec(ctx, "DROP OWNED BY "+pgx.Identifier{name}.Sanitize())
	_, _ = f.super.Exec(ctx, "DROP ROLE "+pgx.Identifier{name}.Sanitize())
}

// principal creates a principal (sponsored or not) and its roles.
func (f *fixture) principal(t *testing.T, ceiling agentguard.Env,
	sponsored bool) agentguard.Principal {
	t.Helper()
	ctx := context.Background()
	var sponsor *int
	if sponsored {
		email := uniq("g1g-sponsor") + "@example.com"
		id, err := auth.CreateUser(ctx, f.super, email, "password-123", "admin")
		require.NoError(t, err)
		sponsor = &id
	}
	p, err := f.store.Create(ctx, agentguard.CreateRequest{
		Name:          fmt.Sprintf("bot-%d-%d", time.Now().UnixNano()%1e9, seq.Add(1)),
		SponsorUserID: sponsor, Profile: "readonly-analyst", EnvCeiling: ceiling,
		CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	kr, err := crypto.NewKeyring(make([]byte, 32))
	require.NoError(t, err)
	rm, err := agentguard.NewRoleManager(f.store, kr, agentguard.DefaultRoleConfig())
	require.NoError(t, err)
	t.Cleanup(func() {
		f.dropRole(p.BrokerRole())
		f.dropRole(p.LoginRole())
		// The registry is database-wide: a later test's reconcile pass must
		// not meet this test's rows.
		_, _ = f.super.Exec(ctx, "DELETE FROM sage.guard_grants WHERE principal_id = $1",
			p.ID)
		_, _ = f.super.Exec(ctx, "DELETE FROM sage.guard_grant_requests "+
			"WHERE principal_id = $1", p.ID)
	})
	_, err = rm.Ensure(ctx, agentguard.RoleRequest{PrincipalID: p.ID,
		Cluster: agentguard.Cluster{Key: "g1g-" + f.db, Admin: f.admin,
			Databases: []agentguard.ClusterDatabase{{Name: f.db, Pool: f.admin}}},
		Approval: agentguard.Approval{ApprovedBy: 1, ApprovalID: 1}, Executor: f.exec})
	require.NoError(t, err)
	return p
}

func (f *fixture) setRuntime(mut func(*policy.RuntimeState)) {
	rt := f.runtime.Load().(policy.RuntimeState)
	mut(&rt)
	f.runtime.Store(rt)
}

// request is a read grant on orders for the fixture principal.
func (f *fixture) request(cols ...string) GrantRequest {
	return GrantRequest{PrincipalID: f.p.ID, Target: f.target, Capability: CapabilityRead,
		Objects:  []ObjectRequest{{Object: f.schema + ".orders", Columns: cols}},
		Duration: time.Hour, Approval: agentguard.Approval{ApprovedBy: 1, ApprovalID: 9},
		Reason: "test"}
}

// can reports has_column_privilege(role, schema.orders, col, SELECT).
func (f *fixture) can(t *testing.T, role, col string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT has_column_privilege($1, $2::regclass, $3, 'SELECT')", role,
		pgx.Identifier{f.schema, "orders"}.Sanitize(), col).Scan(&ok))
	return ok
}

// schemaUsage reports whether role holds USAGE on the app schema directly
// (not through PUBLIC).
func (f *fixture) schemaUsage(t *testing.T, role string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, f.super.QueryRow(context.Background(), `SELECT EXISTS (
		SELECT 1 FROM pg_namespace n, aclexplode(n.nspacl) a
		WHERE n.nspname = $1 AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $2)
		  AND a.privilege_type = 'USAGE')`, f.schema, role).Scan(&ok))
	return ok
}

// actions counts action_log rows of a type for the principal.
func (f *fixture) actions(t *testing.T, actionType string) []string {
	t.Helper()
	rows, err := f.super.Query(context.Background(), `SELECT sql_executed
		FROM sage.action_log WHERE principal_id = $1 AND action_type = $2 ORDER BY id`,
		f.p.ID, actionType)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return out
}

// expireNow moves a grant's expiry into the past (and its granted_at
// before it, for the CHECK).
func (f *fixture) expireNow(t *testing.T, ids ...int64) {
	t.Helper()
	_, err := f.super.Exec(context.Background(), `UPDATE sage.guard_grants
		SET granted_at = now() - interval '2 minutes', expires_at = now() - interval '1 second'
		WHERE id = ANY($1)`, ids)
	require.NoError(t, err)
}

func relationGrant(t *testing.T, gs []Grant) Grant {
	t.Helper()
	for _, g := range gs {
		if g.ObjectKind == KindRelation {
			return g
		}
	}
	t.Fatalf("no relation grant in %+v", gs)
	return Grant{}
}

func denied(t *testing.T, err error, reason agentguard.Reason) *agentguard.DeniedError {
	t.Helper()
	d, ok := agentguard.IsDenied(err)
	if !ok {
		t.Fatalf("want denied %s, got %v", reason, err)
	}
	require.Equal(t, reason, d.Reason)
	return d
}
