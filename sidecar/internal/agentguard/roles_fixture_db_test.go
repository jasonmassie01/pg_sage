package agentguard

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// roleFixture is a cluster where pg_sage runs as a non-superuser role with
// CREATEROLE (what Guard requires), with an executor whose gate the test
// controls.
type roleFixture struct {
	super    *pgxpool.Pool // superuser, for setup and inspection
	admin    *pgxpool.Pool // pg_sage's role
	dsn      string
	db       string
	store    *Store
	manager  *RoleManager
	exec     *executor.Executor
	cluster  Cluster
	runtime  atomic.Value // policy.RuntimeState
	document atomic.Value // policy.Document
}

var adminSeq atomic.Int64

// newRoleFixture skips below PostgreSQL 16 (role management is
// posture-only there; TestEnsure_RefusedBeforePG16 covers it).
func newRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	super := livePool(t)
	if serverVersionNum(t, super) < MinRoleServerVersion {
		t.Skip("role management needs PostgreSQL 16+; " +
			"TestEnsure_RefusedBeforePG16 covers older servers")
	}
	f := &roleFixture{super: super, dsn: testdb.SkipUnlessLive(t), store: NewStore(super)}
	f.db = currentDatabase(t, super)
	f.admin = f.createAdmin(t)
	m, err := NewRoleManager(f.store, testKeyring(t), DefaultRoleConfig())
	require.NoError(t, err)
	f.manager = m
	f.runtime.Store(policy.RuntimeState{ExecutorEnabled: true,
		TrustLevel: policy.TrustAdvisory, ExecutionMode: "auto"})
	f.document.Store(policy.UnattendedProfile())
	f.exec = executor.New(f.admin, config.DefaultConfig(), time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	f.exec.WithPolicyGate(policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return f.runtime.Load().(policy.RuntimeState), nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return f.document.Load().(policy.Document), nil
		},
	}))
	f.cluster = Cluster{Key: "test-cluster-" + f.db, Admin: f.admin,
		Databases: []ClusterDatabase{{Name: f.db, Pool: f.admin}}}
	return f
}

// createAdmin makes pg_sage's role: LOGIN CREATEROLE, CONNECT with grant
// option on the fixture database, and write access to its sage schema.
func (f *roleFixture) createAdmin(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("g1core_admin_%d_%d", time.Now().UnixNano()%1e9, adminSeq.Add(1))
	stmts := []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN CREATEROLE PASSWORD 'admin-pw'", name),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s WITH GRANT OPTION", ident(f.db), name),
		fmt.Sprintf("GRANT USAGE ON SCHEMA sage TO %s", name),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA sage TO %s", name),
		fmt.Sprintf("GRANT USAGE ON ALL SEQUENCES IN SCHEMA sage TO %s", name),
	}
	for _, s := range stmts {
		_, err := f.super.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	pool, err := pgxpool.New(ctx, withUser(t, f.dsn, name, "admin-pw"))
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		f.dropRole(name)
	})
	return pool
}

// dropRole removes a role and everything it holds, as superuser.
func (f *roleFixture) dropRole(name string) {
	ctx := context.Background()
	var exists bool
	_ = f.super.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)",
		name).Scan(&exists)
	if !exists {
		return
	}
	_, _ = f.super.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
		"WHERE usename = $1", name)
	_, _ = f.super.Exec(ctx, "DROP OWNED BY "+ident(name))
	_, _ = f.super.Exec(ctx, "DROP ROLE "+ident(name))
}

// principal creates a sponsored principal whose roles are dropped at the
// end of the test.
func (f *roleFixture) principal(t *testing.T) Principal {
	t.Helper()
	sponsor := createUser(t, f.super, "admin")
	p := newPrincipal(t, f.store, &sponsor)
	t.Cleanup(func() {
		f.dropRole(p.BrokerRole())
		f.dropRole(p.LoginRole())
	})
	return p
}

func (f *roleFixture) request(p Principal) RoleRequest {
	return RoleRequest{PrincipalID: p.ID, Cluster: f.cluster,
		Approval: Approval{ApprovedBy: 1, ApprovalID: 77}, Executor: f.exec}
}

func (f *roleFixture) setRuntime(mut func(*policy.RuntimeState)) {
	rt := f.runtime.Load().(policy.RuntimeState)
	mut(&rt)
	f.runtime.Store(rt)
}

// brokerLogin logs in with the stored broker credential.
func (f *roleFixture) brokerLogin(t *testing.T, p Principal) (string, string, error) {
	t.Helper()
	role, password, err := f.store.BrokerCredential(context.Background(), f.manager.keyring,
		p.ID, f.cluster.Key)
	require.NoError(t, err)
	who, err := tryLogin(context.Background(), withUser(t, f.dsn, role, password))
	return who, password, err
}

func (f *roleFixture) roleExists(t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&ok))
	return ok
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
