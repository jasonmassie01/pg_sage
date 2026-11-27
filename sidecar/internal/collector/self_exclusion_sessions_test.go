package collector

import (
	"context"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// Session self-exclusion (perf v1.8.3, perf-selfexcl). pg_sage's own
// sessions (application_name pg_sage) run its catalog reads, index builds
// and probes; the session inputs of the snapshot (system stats, locks,
// connection states, the load circuit breaker) count the
// application's sessions only. The collector itself runs on a pool
// configured like pg_sage's, as in production, so its own connections are
// pg_sage's too.

// sageCollector is a collector on a pg_sage-configured pool.
func sageCollector(t *testing.T) (*Collector, *pgxpool.Pool) {
	t.Helper()
	testPool(t) // skips when the database is unavailable
	return sageCollectorOn(t, testdb.SkipUnlessLive(t))
}

// sageCollectorOn is sageCollector on the database dsn names.
func sageCollectorOn(t *testing.T, dsn string) (*Collector, *pgxpool.Pool) {
	t.Helper()
	pool := selfload.SagePool(t, dsn)
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return New(pool, testConfig(), version, noopLog), pool
}

func TestSystemStatsCountApplicationSessionsOnly(t *testing.T) {
	c, _ := sageCollector(t)
	selfload.Start(t, testdb.SkipUnlessLive(t))
	s, err := c.collectSystem(context.Background())
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	// Application: holder idle in transaction; waiter and sleeper active.
	if s.IdleInTransaction != 1 {
		t.Errorf("idle_in_transaction = %d, want 1 (the application holder only)",
			s.IdleInTransaction)
	}
	if s.ActiveBackends != 2 {
		t.Errorf("active_backends = %d, want 2 (application waiter and sleeper only)",
			s.ActiveBackends)
	}
	// Connection slots are capacity: pg_sage's sessions occupy them too.
	if s.TotalBackends < 6 {
		t.Errorf("total_backends = %d, want every client slot in use (>= 6)",
			s.TotalBackends)
	}
}

func TestLocksSnapshotLeavesOutPgSageSessions(t *testing.T) {
	c, _ := sageCollector(t)
	_, app, sage := selfload.Start(t, testdb.SkipUnlessLive(t))
	locks, _, err := c.collectLocks(context.Background())
	if err != nil {
		t.Fatalf("collectLocks: %v", err)
	}
	seen := map[int]bool{}
	for _, l := range locks {
		seen[l.PID] = true
	}
	for _, pid := range sage.Pids() {
		if seen[pid] {
			t.Errorf("pg_sage session %d is in the locks snapshot", pid)
		}
	}
	if !seen[app.Holder] || !seen[app.Waiter] {
		t.Errorf("application holder %d / waiter %d missing from %d locks",
			app.Holder, app.Waiter, len(locks))
	}
}

func TestConnectionStatesCountApplicationSessionsOnly(t *testing.T) {
	_, pool := sageCollector(t)
	selfload.Start(t, testdb.SkipUnlessLive(t))
	cs, err := collectConfigSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("collectConfigSnapshot: %v", err)
	}
	byState := map[string]int{}
	for _, s := range cs.ConnectionStates {
		byState[s.State] = s.Count
	}
	if byState["active"] != 2 || byState["idle in transaction"] != 1 {
		t.Fatalf("connection states = %v, want active 2 and idle in transaction 1 "+
			"(the application's sessions)", byState)
	}
}

// advisors-07: the churn sample of live sessions is gone (the advisor reads
// the pg_stat_database.sessions delta instead). The snapshot says whether
// pg_sage's role owns the database, which ALTER DATABASE ... SET needs: the
// test role does; a role without ownership does not.
func TestConfigSnapshotDatabaseOwnership(t *testing.T) {
	_, pool := sageCollector(t)
	ctx := context.Background()
	cs, err := collectConfigSnapshot(ctx, pool)
	if err != nil {
		t.Fatalf("collectConfigSnapshot: %v", err)
	}
	if !cs.OwnsDatabase {
		t.Fatal("the test role owns (or is superuser on) its database: OwnsDatabase false")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sage_not_owner') THEN
			CREATE ROLE sage_not_owner NOLOGIN;
		END IF; END $$`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET ROLE sage_not_owner"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET ROLE") }()
	notOwner, err := collectConfigSnapshot(ctx, conn)
	if err != nil {
		t.Fatalf("collectConfigSnapshot as a non-owner: %v", err)
	}
	if notOwner.OwnsDatabase {
		t.Error("a role that does not own the database: OwnsDatabase true")
	}
}

// The circuit breaker backs off when the server is busy; pg_sage's own
// active sessions are not load it should back off from. The ratio is
// cluster-wide and other test packages share the server, so the check is
// made inside one pg_stat_activity snapshot: the breaker's count must be
// the raw active count minus exactly the pg_sage sessions, and those must
// include the 8 this test started.
func TestCircuitBreakerLoadIgnoresPgSageSessions(t *testing.T) {
	_, pool := sageCollector(t)
	w := selfload.New(t, testdb.SkipUnlessLive(t))
	w.StartApp(t)
	w.StartSage(t, 6) // 8 active pg_sage sessions: waiter, sleeper, 6 more
	var counted, raw, sage float64
	err := pool.QueryRow(context.Background(), `SELECT (`+loadRatioSQL+`) *
		(SELECT setting::float FROM pg_settings WHERE name = 'max_connections'),
		(SELECT count(*) FROM pg_stat_activity
		  WHERE state = 'active' AND pid <> pg_backend_pid()),
		(SELECT count(*) FROM pg_stat_activity
		  WHERE state = 'active' AND pid <> pg_backend_pid()
		    AND NOT (`+selfmonitor.ActivityExclusionSQL("")+`))`).Scan(&counted, &raw, &sage)
	if err != nil {
		t.Fatalf("read load counts: %v", err)
	}
	if sage < 8 {
		t.Fatalf("only %.0f active pg_sage sessions seen, want the test's 8", sage)
	}
	if math.Round(counted) != raw-sage {
		t.Fatalf("load ratio counts %.0f active sessions of %.0f, want %.0f "+
			"(all but the %.0f pg_sage sessions)", counted, raw, raw-sage, sage)
	}
}
