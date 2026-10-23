package upkeep

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// liveCluster is the fixture server with pg_sage running as a
// non-superuser CREATEROLE role, the real role manager and an executor
// whose trust level the test sets: the scheduled jobs end to end (PG16+).
type liveCluster struct {
	super   *pgxpool.Pool
	admin   *pgxpool.Pool
	name    string
	dsn     string
	key     string
	kr      *crypto.Keyring
	manager *agentguard.RoleManager
	exec    *executor.Executor
	trust   string
}

func newLiveCluster(t *testing.T) *liveCluster {
	t.Helper()
	super := livePool(t)
	testdb.RequireServerVersion(t, super, agentguard.MinRoleServerVersion,
		"agent role management")
	testdb.LockAgentRoles(t)
	c := &liveCluster{super: super, dsn: testdb.SkipUnlessLive(t), trust: "advisory"}
	c.key = uniqKey()
	c.admin, c.name = createAdmin(t, super, c.dsn)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	kr, err := crypto.NewKeyring(key)
	require.NoError(t, err)
	c.kr = kr
	c.manager, err = agentguard.NewRoleManager(agentguard.NewStore(super), kr,
		agentguard.DefaultRoleConfig())
	require.NoError(t, err)
	c.exec = executor.New(c.admin, config.DefaultConfig(), time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	c.exec.WithPolicyGate(policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true,
				TrustLevel: c.trust, ExecutionMode: "auto"}, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return policy.UnattendedProfile(), nil
		},
	}))
	return c
}

// createAdmin makes pg_sage's role on the fixture database.
func createAdmin(t *testing.T, super *pgxpool.Pool, dsn string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	var db string
	require.NoError(t, super.QueryRow(ctx, "SELECT current_database()").Scan(&db))
	name := fmt.Sprintf("upkeep_admin_%d_%d", time.Now().UnixNano()%1e9, seq.Add(1))
	for _, s := range []string{
		"CREATE ROLE " + name + " LOGIN CREATEROLE PASSWORD 'admin-pw'",
		fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO %s WITH GRANT OPTION`, db, name),
		"GRANT USAGE ON SCHEMA sage TO " + name,
		"GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA sage TO " + name,
		"GRANT USAGE ON ALL SEQUENCES IN SCHEMA sage TO " + name,
	} {
		_, err := super.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(name, "admin-pw")
	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		bg := context.Background()
		for _, s := range []string{"DROP OWNED BY " + name, "DROP ROLE " + name} {
			if _, err := super.Exec(bg, s); err != nil {
				t.Errorf("cleanup of %s: %s: %v", name, s, err)
			}
		}
	})
	return pool, name
}

func (c *liveCluster) cluster() agentguard.Cluster {
	var db string
	_ = c.super.QueryRow(context.Background(), "SELECT current_database()").Scan(&db)
	return agentguard.Cluster{Key: c.key, Admin: c.admin,
		Databases: []agentguard.ClusterDatabase{{Name: db, Pool: c.admin}}}
}

func (c *liveCluster) target() agentguard.KillTarget {
	cl := c.cluster()
	return agentguard.KillTarget{Name: cl.Databases[0].Name, Pool: c.admin,
		ClusterKey: c.key, Executor: c.exec}
}

// ensure creates p's roles under approver's approval and drops them at the
// end of the test if they are still there.
func (c *liveCluster) ensure(t *testing.T, p agentguard.Principal, approver int) int64 {
	t.Helper()
	res, err := c.manager.Ensure(context.Background(), agentguard.RoleRequest{
		PrincipalID: p.ID, Cluster: c.cluster(), Executor: c.exec,
		Approval: agentguard.Approval{ApprovedBy: approver, ApprovalID: 5}})
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, role := range []string{p.BrokerRole(), p.LoginRole()} {
			c.dropIfPresent(t, role)
		}
	})
	return res.ActionID
}

func (c *liveCluster) dropIfPresent(t *testing.T, role string) {
	bg := context.Background()
	if !c.roleExists(t, role) {
		return
	}
	for _, s := range []string{
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '" + role + "'",
		"GRANT " + role + " TO " + c.name + " WITH INHERIT TRUE",
		"SET ROLE " + c.name, "DROP OWNED BY " + role, "RESET ROLE",
		"DROP OWNED BY " + role, "DROP ROLE " + role,
	} {
		if _, err := c.super.Exec(bg, s); err != nil {
			t.Errorf("cleanup of %s: %s: %v", role, s, err)
			return
		}
	}
}

func (c *liveCluster) roleExists(t *testing.T, role string) bool {
	var ok bool
	require.NoError(t, c.super.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&ok))
	return ok
}

func (c *liveCluster) loginWorks(t *testing.T, role, password string) bool {
	u, err := url.Parse(c.dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return false
	}
	_ = conn.Close(context.Background())
	return true
}

func (c *liveCluster) runner(t *testing.T) *Runner {
	r, err := New(c.super, c.manager, targetsOf(c.target()), weekly())
	require.NoError(t, err)
	return r
}

// lastAction reads the newest action_log row of a type for p.
func (c *liveCluster) lastAction(t *testing.T, actionType, pid string) (int64, int,
	map[string]any) {
	var id int64
	var by int
	var raw []byte
	require.NoError(t, c.super.QueryRow(context.Background(), `SELECT id, approved_by,
		after_state FROM sage.action_log WHERE action_type = $1 AND principal_id = $2
		ORDER BY id DESC LIMIT 1`, actionType, pid).Scan(&id, &by, &raw))
	var after map[string]any
	require.NoError(t, json.Unmarshal(raw, &after))
	return id, by, after
}

func TestEndToEnd_RotationSetsANewPasswordAndSaysItIsScheduled(t *testing.T) {
	c := newLiveCluster(t)
	op := createUser(t, c.super, "operator")
	p := newPrincipal(t, c.super, op)
	firstID := c.ensure(t, p, op)
	store := agentguard.NewStore(c.super)
	ctx := context.Background()
	role, oldPW, err := store.BrokerCredential(ctx, c.kr, p.ID, c.key)
	require.NoError(t, err)
	require.True(t, c.loginWorks(t, role, oldPW), "the first password works")
	_, err = c.super.Exec(ctx, `UPDATE sage.guard_cluster_roles SET rotated_at = now() -
		interval '8 days' WHERE principal_id = $1`, p.ID)
	require.NoError(t, err)

	rep, err := c.runner(t).RotateBroker(ctx, Fence{})
	require.NoError(t, err)
	_, ok := outcomeFor(rep.Done, p.ID)
	require.True(t, ok, "%+v", rep)
	_, newPW, err := store.BrokerCredential(ctx, c.kr, p.ID, c.key)
	require.NoError(t, err)
	require.NotEqual(t, oldPW, newPW)
	require.False(t, c.loginWorks(t, role, oldPW), "the old password fails")
	require.True(t, c.loginWorks(t, role, newPW), "the new password works")
	_, by, after := c.lastAction(t, executor.ActionTypeGuardRoleEnsure, p.ID)
	require.Equal(t, op, by)
	require.Equal(t, JobBrokerRotation, after["scheduled"])
	orig, _ := after["original_approval"].(map[string]any)
	require.Equal(t, float64(firstID), orig["original_action_id"])
	require.Equal(t, float64(op), orig["original_approved_by"])
}

func TestEndToEnd_RetireDropsTheRolesAfterGrace(t *testing.T) {
	c := newLiveCluster(t)
	admin := createUser(t, c.super, "admin")
	p := newPrincipal(t, c.super, admin)
	c.ensure(t, p, admin)
	retireAgo(t, c.super, p, admin, week+time.Hour)
	ctx := context.Background()

	rep, err := c.runner(t).DropRetired(ctx, Fence{})
	require.NoError(t, err)
	_, ok := outcomeFor(rep.Done, p.ID)
	require.True(t, ok, "%+v", rep)
	require.False(t, c.roleExists(t, p.BrokerRole()))
	require.False(t, c.roleExists(t, p.LoginRole()))
	_, by, after := c.lastAction(t, executor.ActionTypeGuardRoleRetire, p.ID)
	require.Equal(t, admin, by)
	require.Equal(t, JobRetireGrace, after["scheduled"])
	roles, err := agentguard.NewStore(c.super).ClusterRoles(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, agentguard.RoleStatusRetired, roles[0].Status)

	// Retired roles are not selected again.
	rep, err = c.runner(t).DropRetired(ctx, Fence{})
	require.NoError(t, err)
	_, ok = outcomeFor(rep.Done, p.ID)
	require.False(t, ok, "%+v", rep)
}

func TestEndToEnd_ObservationWithholdsRotation(t *testing.T) {
	c := newLiveCluster(t)
	op := createUser(t, c.super, "operator")
	p := newPrincipal(t, c.super, op)
	c.ensure(t, p, op)
	ctx := context.Background()
	_, err := c.super.Exec(ctx, `UPDATE sage.guard_cluster_roles SET rotated_at = now() -
		interval '8 days' WHERE principal_id = $1`, p.ID)
	require.NoError(t, err)
	_, before, err := agentguard.NewStore(c.super).BrokerCredential(ctx, c.kr, p.ID, c.key)
	require.NoError(t, err)
	c.trust = "observation"

	rep, err := c.runner(t).RotateBroker(ctx, Fence{})
	require.NoError(t, err)
	o, ok := outcomeFor(rep.Withheld, p.ID)
	require.True(t, ok, "a non-narrowing rotation is withheld at observation: %+v", rep)
	require.True(t, o.Reason != "", "the gate's reason is reported")
	_, after, err := agentguard.NewStore(c.super).BrokerCredential(ctx, c.kr, p.ID, c.key)
	require.NoError(t, err)
	require.Equal(t, before, after, "nothing rotated")
}
