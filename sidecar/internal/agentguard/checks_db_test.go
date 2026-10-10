package agentguard

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// openSession logs in as role and keeps the session until the test ends.
func openSession(t *testing.T, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
}

func findingFor(fs []BackendFinding, role string) (BackendFinding, bool) {
	for _, f := range fs {
		if f.Role == role {
			return f, true
		}
	}
	return BackendFinding{}, false
}

func TestCheckBackends_UnregisteredAgentRole(t *testing.T) {
	pool := livePool(t)
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	role := BrokerRoleName(uniqName("rogue"))
	_, err := pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'rogue-pw' CREATEDB")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) "+
			"FROM pg_stat_activity WHERE usename = $1", role)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	openSession(t, withUser(t, dsn, role, "rogue-pw"))
	found, err := CheckBackends(ctx, pool, NewStore(pool), "any-cluster")
	require.NoError(t, err)
	f, ok := findingFor(found, role)
	require.True(t, ok, "G1-01 flags the connected rogue role: %+v", found)
	require.Equal(t, 1, f.Sessions)
	require.Contains(t, f.Problems, "is not a registered agent role")
	require.Contains(t, f.Problems, "has CREATEDB")
}

func TestCheckBackends_RegisteredRolePassesThenDrifts(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	res, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	require.Empty(t, res.Ownership, "G1-07: AP-02 is zero after the ensure")
	_, password, err := f.brokerLogin(t, p)
	require.NoError(t, err)
	openSession(t, withUser(t, f.dsn, p.BrokerRole(), password))
	found, err := CheckBackends(ctx, f.super, f.store, f.cluster.Key)
	require.NoError(t, err)
	_, flagged := findingFor(found, p.BrokerRole())
	require.False(t, flagged, "a role to spec passes: %+v", found)
	_, err = f.super.Exec(ctx, "ALTER ROLE "+ident(p.BrokerRole())+" BYPASSRLS")
	require.NoError(t, err)
	require.NoError(t, f.store.SetClusterRoleStatus(ctx, p.ID, f.cluster.Key,
		RoleStatusKilled, []byte(`{"login":true}`)))
	found, err = CheckBackends(ctx, f.super, f.store, f.cluster.Key)
	require.NoError(t, err)
	got, flagged := findingFor(found, p.BrokerRole())
	require.True(t, flagged)
	require.Contains(t, got.Problems, "has BYPASSRLS")
	require.Contains(t, got.Problems, "is registered as killed but connected")
	other, err := CheckBackends(ctx, f.super, f.store, "another-cluster")
	require.NoError(t, err)
	got, _ = findingFor(other, p.BrokerRole())
	require.Contains(t, got.Problems, "is not a registered agent role",
		"registration is per cluster")
	roles, err := f.store.ClusterRoles(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, `{"login": true}`, string(roles[0].PriorAttrs))
}

func TestSelfCheck_OnPgSagesOwnRole(t *testing.T) {
	pool := livePool(t)
	res, err := SelfCheck(context.Background(), pool)
	require.NoError(t, err)
	require.True(t, res.Superuser, "the test server's role is superuser")
	require.ErrorIs(t, res.Err(), ErrSelfCheck)
	require.Empty(t, res.Inherits, "a superuser's inherited list is not meaningful")
}

func TestAgentOwnership_G107(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	role := LoginRoleName(uniqName("owner"))
	table := "public.g1core_owned_" + strings.TrimPrefix(role, "sage_agent_")
	for _, s := range []string{"CREATE ROLE " + role + " NOLOGIN",
		"CREATE TABLE " + table + " (id int)", "ALTER TABLE " + table + " OWNER TO " + role} {
		_, err := pool.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	found, err := AgentOwnership(ctx, pool)
	require.NoError(t, err)
	hit := false
	for _, f := range found {
		hit = hit || f.Object == role
	}
	require.True(t, hit, "AP-02 sees the owned table: %+v", found)
	err = VerifyNoAgentOwnership(ctx, pool)
	require.ErrorIs(t, err, ErrPostCheck)
	require.ErrorContains(t, err, role)
	_, err = pool.Exec(ctx, "ALTER TABLE "+table+" OWNER TO CURRENT_USER")
	require.NoError(t, err)
	found, err = AgentOwnership(ctx, pool)
	require.NoError(t, err)
	for _, f := range found {
		require.NotEqual(t, role, f.Object)
	}
	_, err = AgentOwnership(ctx, nil)
	require.ErrorIs(t, err, ErrInvalid)
}
