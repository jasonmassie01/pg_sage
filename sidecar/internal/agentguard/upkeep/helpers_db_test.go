package upkeep

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard/upkeep"))
}

// fakeRoles records the role contracts a pass asks for and answers with
// the configured error (nil: success with action id 100+n).
type fakeRoles struct {
	mu      sync.Mutex
	ensures []agentguard.RoleRequest
	retires []agentguard.RoleRequest
	err     error
}

func (f *fakeRoles) Ensure(_ context.Context, req agentguard.RoleRequest) (
	agentguard.RoleResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, req)
	return agentguard.RoleResult{ActionID: int64(100 + len(f.ensures)), Rotated: true}, f.err
}

func (f *fakeRoles) Retire(_ context.Context, req agentguard.RoleRequest) (
	agentguard.RoleResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retires = append(f.retires, req)
	return agentguard.RoleResult{ActionID: int64(200 + len(f.retires))}, f.err
}

func (f *fakeRoles) calls() (ensures, retires []agentguard.RoleRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentguard.RoleRequest{}, f.ensures...),
		append([]agentguard.RoleRequest{}, f.retires...)
}

// livePool is a superuser pool on the package's fixture database, with
// the sage schema bootstrapped.
func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, schema.Bootstrap(ctx, pool))
	return pool
}

// agentRoleLocks makes lockAgentRoles re-entrant within one test.
var agentRoleLocks sync.Map // *testing.T -> struct{}

// lockAgentRoles holds the cluster-wide agent roles lock for the test:
// agent roles are cluster-wide and other packages' tests assume none exist.
func lockAgentRoles(t *testing.T) {
	t.Helper()
	if _, held := agentRoleLocks.LoadOrStore(t, struct{}{}); held {
		return
	}
	release, err := testdb.LockCluster(context.Background(), os.Getenv(testdb.EnvName),
		testdb.AgentRolesLock)
	if err != nil {
		agentRoleLocks.Delete(t)
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		release()
		agentRoleLocks.Delete(t)
	})
}

var seq atomic.Int64

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e9, seq.Add(1))
}

// uniqKey is a cluster key no other test uses.
func uniqKey() string { return uniq("cluster") }

func createUser(t *testing.T, pool *pgxpool.Pool, role string) int {
	t.Helper()
	id, err := auth.CreateUser(context.Background(), pool, uniq("upkeep-"+role)+
		"@example.com", "password-123", role)
	require.NoError(t, err)
	return id
}

func newPrincipal(t *testing.T, pool *pgxpool.Pool, sponsor int) agentguard.Principal {
	t.Helper()
	p, err := agentguard.NewStore(pool).Create(context.Background(),
		agentguard.CreateRequest{Name: uniq("bot"), SponsorUserID: &sponsor,
			Profile: "readonly-analyst", EnvCeiling: agentguard.EnvDev,
			CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	return p
}

// registerRoles inserts a principal's cluster roles row directly (the
// sealed credential is a placeholder: these tests never open it), with
// rotated_at age ago.
func registerRoles(t *testing.T, pool *pgxpool.Pool, p agentguard.Principal, key,
	status string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.guard_cluster_roles
		(principal_id, cluster_key, login_role, broker_role, broker_secret_ct, key_id,
		 status, rotated_at)
		VALUES ($1, $2, $3, $4, '\x00', 'k1', $5, now() - $6::interval)`,
		p.ID, key, p.LoginRole(), p.BrokerRole(), status, age.String())
	require.NoError(t, err)
}

// retireAgo retires p as admin (0: no recorded admin) and backdates the
// retirement by age.
func retireAgo(t *testing.T, pool *pgxpool.Pool, p agentguard.Principal, admin int,
	age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if admin > 0 {
		require.NoError(t, RecordRetiringAdmin(ctx, pool, p.ID, admin))
	}
	_, err := agentguard.NewStore(pool).SetStatus(ctx, p.ID, agentguard.StatusRetired,
		"no longer needed")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE sage.guard_principals SET updated_at = now() - $2::interval
		WHERE id = $1`, p.ID, age.String())
	require.NoError(t, err)
}

// target is a monitored database of cluster key with a (fake) executor.
func target(pool *pgxpool.Pool, key string) agentguard.KillTarget {
	return agentguard.KillTarget{Name: "db-" + key, Pool: pool, ClusterKey: key,
		Executor: fakeApplier{name: key}}
}

func targetsOf(ts ...agentguard.KillTarget) Targets {
	return func(context.Context) ([]agentguard.KillTarget, error) { return ts, nil }
}

func runner(t *testing.T, pool *pgxpool.Pool, roles Roles, cfg Config,
	ts ...agentguard.KillTarget) *Runner {
	t.Helper()
	r, err := New(pool, roles, targetsOf(ts...), cfg)
	require.NoError(t, err)
	return r
}

const week = 7 * 24 * time.Hour

func weekly() Config { return Config{RetireGrace: week, Rotation: week, Batch: 1000} }

// findingOf reads the open finding of category and identifier.
func findingOf(t *testing.T, pool *pgxpool.Pool, category, ident string) (
	severity, title, sql string, open bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT severity, title,
		COALESCE(recommended_sql, '') FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
		category, ident).Scan(&severity, &title, &sql)
	if err != nil {
		return "", "", "", false
	}
	return severity, title, sql, true
}

func findingCount(t *testing.T, pool *pgxpool.Pool, category, ident, status string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM
		sage.findings WHERE category = $1 AND object_identifier = $2 AND status = $3`,
		category, ident, status).Scan(&n))
	return n
}

// ensureLogged records a successful guard_role_ensure for p on key in
// pool's action_log, approved by approver, and returns its id.
func ensureLogged(t *testing.T, pool *pgxpool.Pool, p agentguard.Principal, key string,
	approver int, approvalID int64, age time.Duration) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, before_state, after_state, outcome, approved_by,
		 approved_at, principal_id, approval_id, executed_at)
		VALUES ('guard_role_ensure', 'CREATE ROLE', jsonb_build_object('principal_id', $1::text,
		 'cluster_key', $2::text), '{}', 'success', $3, now(), $1, NULLIF($4::bigint, 0),
		 now() - $5::interval)
		RETURNING id`, p.ID, key, approver, approvalID, age.String()).Scan(&id))
	return id
}

// takeLease makes holder the leader of scope at epoch; it returns the fence.
func takeLease(t *testing.T, pool *pgxpool.Pool, scope string) Fence {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.fleet_leader_lease
		(scope, holder, epoch, expires_at) VALUES ($1, 'me', 3, now() + interval '1 minute')
		ON CONFLICT (scope) DO UPDATE SET holder = 'me', epoch = 3,
		expires_at = now() + interval '1 minute'`, scope)
	require.NoError(t, err)
	return Fence{Scope: scope, Holder: "me", Epoch: 3}
}
