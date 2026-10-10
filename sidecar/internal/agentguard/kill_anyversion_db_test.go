package agentguard

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// manualRole creates an agent-named role by hand with CONNECT on db; it
// is dropped (its grant revoked first) at the end of the test.
func manualRole(t *testing.T, super *pgxpool.Pool, db string) string {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("sage_agentb_%s", strings.Repeat("m", 10))
	_, err := super.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'manual-pw'")
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = super.Exec(bg, "SELECT pg_terminate_backend(pid) "+
			"FROM pg_stat_activity WHERE usename = $1", role)
		_, _ = super.Exec(bg, "REVOKE CONNECT ON DATABASE "+ident(db)+" FROM "+role)
		_, _ = super.Exec(bg, "DROP ROLE "+role)
	})
	_, err = super.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(db)+" TO "+role)
	require.NoError(t, err)
	return role
}

// optionalReplica is the configured replica of the topology env, if set.
func optionalReplica(t *testing.T, db, role string) (*pgxpool.Pool, string, []Replica) {
	t.Helper()
	rURL := envOr(envKillReplica, "")
	if rURL == "" {
		return nil, "", nil
	}
	dsn := withDatabase(t, rURL, db)
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	replayed(t, dsn, role, "manual-pw")
	return pool, dsn, []Replica{{Name: envOr(envKillReplicaName, "replica1"), DSN: dsn}}
}

// The kill switch on every supported version, PG14 and PG15 included,
// where Guard does not manage roles (§6.6) but agent-named roles may still
// exist (created by hand or before G1): a fleet kill disables them and ends
// their sessions on the primary and a configured replica, through the
// direct path when no executor is wired. With the replica topology env
// set, the replica is checked too.
func TestKill_AnyVersionManualAgentRoles(t *testing.T) {
	super := livePool(t)
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	db := currentDatabase(t, super)
	role := manualRole(t, super, db)
	f := &killFixture{roleFixture: &roleFixture{super: super, dsn: dsn, db: db}}
	replica, replicaDSN, replicas := optionalReplica(t, db, role)
	target := KillTarget{Name: db, Pool: super, ClusterKey: "any-version-" + db,
		Replicas: replicas}
	log := NewFallbackLog(filepath.Join(t.TempDir(), "kill.log"))
	sw, err := NewSwitch(KillDeps{Store: NewStore(super), Fallback: log,
		Config: DefaultKillConfig(),
		Targets: func(context.Context) ([]KillTarget, error) {
			return []KillTarget{target}, nil
		}})
	require.NoError(t, err)
	onPrimary := f.sleepAs(t, super, dsn, role, "manual-pw", 30)
	var onReplica *brokerStatement
	if replica != nil {
		onReplica = f.sleepAs(t, replica, replicaDSN, role, "manual-pw", 30)
	}
	begin := time.Now()
	rep, err := sw.Kill(ctx, KillRequest{Scope: KillScopeAll, Reason: "any version",
		Actor: "admin@example.com"})
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, onPrimary, 10*time.Second)))
	if onReplica != nil {
		require.True(t, terminatedOrCancelled(waitDone(t, onReplica, 10*time.Second)))
		require.True(t, eventuallyLoginFails(t, f, replicaDSN, role, "manual-pw",
			5*time.Second))
	}
	require.Less(t, time.Since(begin), 10*time.Second)
	require.True(t, f.loginFails(t, dsn, role, "manual-pw"))
	require.True(t, rep.Databases[0].Direct, "no executor: the direct path")
	require.GreaterOrEqual(t, rep.Databases[0].RolesDisabled, 1)
	entries, err := log.Entries()
	require.NoError(t, err)
	require.NotEmpty(t, entries)
}
