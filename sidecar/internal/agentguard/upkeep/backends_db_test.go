package upkeep

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentRole creates an agent-named LOGIN role that may connect to the
// fixture database; it is dropped at the end of the test.
func agentRole(t *testing.T, pool *pgxpool.Pool, lane string) string {
	t.Helper()
	lockAgentRoles(t)
	ctx := context.Background()
	b := make([]byte, 10)
	_, err := rand.Read(b)
	require.NoError(t, err)
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	role := lane + sb.String()
	var db string
	require.NoError(t, pool.QueryRow(ctx, "SELECT current_database()").Scan(&db))
	_, err = pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'agent-pw'")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO %s`, db, role))
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
			"WHERE usename = $1", role)
		if _, err := pool.Exec(bg, fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM %s`,
			db, role)); err != nil {
			t.Errorf("cleanup: revoke connect from %s: %v", role, err)
		}
		if _, err := pool.Exec(bg, "DROP ROLE "+role); err != nil {
			t.Errorf("cleanup: drop role %s: %v", role, err)
		}
	})
	return role
}

// connectAs opens a session as role and returns its closer.
func connectAs(t *testing.T, role string) func() {
	t.Helper()
	u, err := url.Parse(testdb.SkipUnlessLive(t))
	require.NoError(t, err)
	u.User = url.UserPassword(role, "agent-pw")
	conn, err := pgx.Connect(context.Background(), u.String())
	require.NoError(t, err)
	return func() { _ = conn.Close(context.Background()) }
}

// waitGone waits until role has no backend.
func waitGone(t *testing.T, pool *pgxpool.Pool, role string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*)
			FROM pg_stat_activity WHERE usename = $1`, role).Scan(&n))
		if n == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s still connected after 10s", role)
}

func violationOf(rep BackendReport, key, role string) (agentguard.BackendFinding, bool) {
	for _, v := range rep.Violations {
		if v.ClusterKey != key {
			continue
		}
		for _, f := range v.Findings {
			if f.Role == role {
				return f, true
			}
		}
	}
	return agentguard.BackendFinding{}, false
}

func TestCheckBackends_UnregisteredRoleRaisesAndResolvesACriticalFinding(t *testing.T) {
	pool := livePool(t)
	role := agentRole(t, pool, "sage_agentb_")
	key := uniqKey()
	r := runner(t, pool, &fakeRoles{}, weekly(), target(pool, key))
	ctx := context.Background()
	closeConn := connectAs(t, role)

	rep, err := r.CheckBackends(ctx, Fence{})
	require.NoError(t, err)
	f, ok := violationOf(rep, key, role)
	require.True(t, ok, "the unregistered agent backend is a violation: %+v", rep)
	require.Equal(t, 1, f.Sessions)
	require.Contains(t, strings.Join(f.Problems, ";"), "not a registered agent role")
	sev, title, _, open := findingOf(t, pool, BackendFindingCategory, role)
	require.True(t, open, "G1-01 at runtime raises a finding")
	require.Equal(t, "critical", sev)
	require.Contains(t, title, role)

	// A second pass refreshes the same finding, never duplicates it.
	_, err = r.CheckBackends(ctx, Fence{})
	require.NoError(t, err)
	require.Equal(t, 1, findingCount(t, pool, BackendFindingCategory, role, "open"))

	closeConn()
	waitGone(t, pool, role)
	rep, err = r.CheckBackends(ctx, Fence{})
	require.NoError(t, err)
	_, ok = violationOf(rep, key, role)
	require.False(t, ok, "%+v", rep)
	require.Equal(t, 0, findingCount(t, pool, BackendFindingCategory, role, "open"))
	require.Equal(t, 1, findingCount(t, pool, BackendFindingCategory, role, "resolved"))
}

func TestCheckBackends_RegisteredCleanRoleIsNoFinding(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	p := newPrincipal(t, pool, sponsor)
	key := uniqKey()
	role := p.BrokerRole()
	lockAgentRoles(t)
	ctx := context.Background()
	var db string
	require.NoError(t, pool.QueryRow(ctx, "SELECT current_database()").Scan(&db))
	_, err := pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'agent-pw'")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO %s`, db, role))
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
			"WHERE usename = $1", role)
		_, _ = pool.Exec(bg, fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM %s`, db, role))
		if _, err := pool.Exec(bg, "DROP ROLE "+role); err != nil {
			t.Errorf("cleanup: drop role %s: %v", role, err)
		}
	})
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	defer connectAs(t, role)()

	rep, err := runner(t, pool, &fakeRoles{}, weekly(), target(pool, key)).
		CheckBackends(ctx, Fence{})
	require.NoError(t, err)
	_, ok := violationOf(rep, key, role)
	require.False(t, ok, "a registered, plain agent role passes G1-01: %+v", rep)
	require.Equal(t, 0, findingCount(t, pool, BackendFindingCategory, role, "open"))

	// The same role connected while its registration says killed is a
	// violation (a kill that did not hold).
	require.NoError(t, agentguard.NewStore(pool).SetClusterRoleStatus(ctx, p.ID, key,
		agentguard.RoleStatusKilled, nil))
	rep, err = runner(t, pool, &fakeRoles{}, weekly(), target(pool, key)).
		CheckBackends(ctx, Fence{})
	require.NoError(t, err)
	f, ok := violationOf(rep, key, role)
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.Join(f.Problems, ";"), "killed")
}

func TestCheckBackends_BypassRLSAgentRoleIsAViolation(t *testing.T) {
	pool := livePool(t)
	role := agentRole(t, pool, "sage_agent_")
	ctx := context.Background()
	_, err := pool.Exec(ctx, "ALTER ROLE "+role+" BYPASSRLS")
	require.NoError(t, err)
	defer connectAs(t, role)()
	key := uniqKey()
	rep, err := runner(t, pool, &fakeRoles{}, weekly(), target(pool, key)).
		CheckBackends(ctx, Fence{})
	require.NoError(t, err)
	f, ok := violationOf(rep, key, role)
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.ToLower(strings.Join(f.Problems, ";")), "bypassrls")
}

func TestCheckBackends_UnknownClusterKeyIsReportedNotGuessed(t *testing.T) {
	pool := livePool(t)
	noKey := agentguard.KillTarget{Name: "nokey", Pool: pool}
	rep, err := runner(t, pool, &fakeRoles{}, weekly(), noKey).
		CheckBackends(context.Background(), Fence{})
	require.NoError(t, err)
	require.Equal(t, 0, rep.Clusters)
	require.Contains(t, rep.Failed["nokey"], "cluster")
	require.Equal(t, 0, len(rep.Violations))
}

func TestCheckBackends_FencedOff(t *testing.T) {
	pool := livePool(t)
	role := agentRole(t, pool, "sage_agentb_")
	defer connectAs(t, role)()
	fence := takeLease(t, pool, uniq("scope"))
	fence.Epoch = 99
	_, err := runner(t, pool, &fakeRoles{}, weekly(), target(pool, uniqKey())).
		CheckBackends(context.Background(), fence)
	require.ErrorIs(t, err, ErrFenced)
	require.Equal(t, 0, findingCount(t, pool, BackendFindingCategory, role, "open"),
		"a fenced-off leader writes no finding")
}

func TestCheckBackends_TargetsUnavailable(t *testing.T) {
	pool := livePool(t)
	r, err := New(pool, &fakeRoles{}, func(context.Context) ([]agentguard.KillTarget, error) {
		return nil, fmt.Errorf("fleet not registered yet")
	}, weekly())
	require.NoError(t, err)
	_, err = r.CheckBackends(context.Background(), Fence{})
	require.ErrorContains(t, err, "fleet not registered yet")
}
