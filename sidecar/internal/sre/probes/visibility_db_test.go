package probes

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// CHECK-08 (Sage SRE M4): without pg_read_all_stats, pg_stat_activity
// hides other roles' state, wait events, transaction start and backend
// type. The activity probes would then report "no lock waits", "no long
// transactions" and an undercount of connections: a healthy zero from a
// missing privilege. They must instead be no_privilege, naming the role.
// Probes whose views every role can read keep working.

// activityProbes are the probes that need pg_read_all_stats. The M6
// probes read other sessions too: LWLock waits and the standby's longest
// query (pg_stat_activity), the xmin holders and the busy autovacuum
// workers of the XID runway (pg_stat_activity), and other roles' spilling
// statements (pg_stat_statements hides their query ids).
var activityProbes = []ID{LockChains, LockGraph, LongTransactions, BackendIdentity,
	ConnectionSaturation, ReplicationLag, VacuumProgress, LWLockWaits, StandbyReplayState,
	TempSpillStatements, XIDRunwayProbe, XminHorizon}

func TestCatalog_ActivityProbesDeclareTheirRole(t *testing.T) {
	need := map[ID]bool{}
	for _, id := range activityProbes {
		need[id] = true
	}
	for _, id := range Catalog().IDs() {
		spec, _ := Catalog().Spec(id)
		got := fmt.Sprint(spec.Requires)
		want := "[]"
		if need[id] {
			want = "[" + RoleReadAllStats + "]"
		}
		if got != want {
			t.Errorf("%s requires %s, want %s", id, got, want)
		}
	}
}

func TestNewRegistry_RejectsAnUnknownRequiredRole(t *testing.T) {
	s := testSpec("bad_role", "SELECT 1 LIMIT $1", func(s *Spec) {
		s.Requires = []string{"pg_write_all_data"}
	})
	if _, err := NewRegistry(s); err == nil {
		t.Fatal("a spec requiring a role outside the allowed set was accepted")
	}
}

// restrictedPool connects as a fresh role with only LOGIN; grant adds
// predefined roles to it.
func restrictedPool(t *testing.T, ctx context.Context, admin *pgxpool.Pool) (*pgxpool.Pool,
	func(role string)) {
	t.Helper()
	role := fmt.Sprintf("sre_probe_vis_%d_%d", os.Getpid(), time.Now().UnixNano()%100000)
	ident := pgx.Identifier{role}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+ident+
		" LOGIN PASSWORD 'sre-vis-test'"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident) })
	u, _ := url.Parse(os.Getenv(testdb.EnvName))
	u.User = url.UserPassword(role, "sre-vis-test")
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect restricted: %v", err)
	}
	t.Cleanup(pool.Close)
	grant := func(r string) {
		if _, err := admin.Exec(ctx, "GRANT "+pgx.Identifier{r}.Sanitize()+" TO "+
			ident); err != nil {
			t.Fatalf("grant %s: %v", r, err)
		}
		pool.Reset()
	}
	return pool, grant
}

func runAs(ctx context.Context, pool *pgxpool.Pool, id ID) Result {
	args := Args{}
	if id == BackendIdentity {
		args = Args{PID: 1, BackendStart: time.Now()}
	}
	return NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, id, args)
}

func TestRunner_ActivityProbesWithoutReadAllStatsAreNoPrivilege(t *testing.T) {
	admin, ctx := livePool(t)
	pool, _ := restrictedPool(t, ctx, admin)
	for _, id := range activityProbes {
		res := runAs(ctx, pool, id)
		if res.Status != StatusNoPrivilege || res.Reason != ReasonMissingRole ||
			len(res.Rows) != 0 {
			t.Errorf("%s without pg_read_all_stats = %s %q (%d rows), want no_privilege %q",
				id, res.Status, res.Reason, len(res.Rows), ReasonMissingRole)
		}
	}
	var visible []string
	for _, id := range []ID{PreparedXacts, ReplicationSlots, WALCheckpoint, Archiver} {
		if res := runAs(ctx, pool, id); res.Status.Usable() {
			visible = append(visible, string(id))
		} else {
			t.Errorf("%s needs no predefined role but returned %s %q (%s)", id,
				res.Status, res.Reason, res.Error)
		}
	}
	sort.Strings(visible)
	if len(visible) != 4 {
		t.Fatalf("observed %v", visible)
	}
}

func TestRunner_PgMonitorGrantsTheActivityProbes(t *testing.T) {
	admin, ctx := livePool(t)
	pool, grant := restrictedPool(t, ctx, admin)
	grant("pg_monitor")
	for _, id := range activityProbes {
		res := runAs(ctx, pool, id)
		if spec, _ := Catalog().Spec(id); spec.Extension != "" &&
			res.Status == StatusUnsupported && res.Reason == "extension_not_installed" {
			continue // the role check passed; the fixture lacks the extension
		}
		if !res.Status.Usable() {
			t.Errorf("%s with pg_monitor = %s %q (%s), want an observation", id,
				res.Status, res.Reason, res.Error)
		}
	}
}
