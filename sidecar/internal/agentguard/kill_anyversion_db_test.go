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
	role := fmt.Sprintf("sage_agentb_%s", strings.Repeat("m", 10))
	_, err := super.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'manual-pw'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), "SELECT pg_terminate_backend(pid) "+
			"FROM pg_stat_activity WHERE usename = $1", role)
		_, _ = super.Exec(context.Background(), "REVOKE CONNECT ON DATABASE "+ident(db)+
			" FROM "+role)
		_, _ = super.Exec(context.Background(), "DROP ROLE "+role)
	})
	_, err = super.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(db)+" TO "+role)
	require.NoError(t, err)
	f := &killFixture{roleFixture: &roleFixture{super: super, dsn: dsn, db: db}}
	target := KillTarget{Name: db, Pool: super, ClusterKey: "any-version-" + db}
	var replica *pgxpool.Pool
	var replicaDSN string
	if rURL := envOr(envKillReplica, ""); rURL != "" {
		replicaDSN = withDatabase(t, rURL, db)
		target.Replicas = []Replica{{Name: envOr(envKillReplicaName, "replica1"),
			DSN: replicaDSN}}
		replica, err = pgxpool.New(ctx, replicaDSN)
		require.NoError(t, err)
		t.Cleanup(replica.Close)
		replayed(t, replicaDSN, role, "manual-pw")
	}
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
