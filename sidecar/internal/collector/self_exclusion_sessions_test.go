package collector

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// Session self-exclusion (perf v1.8.3, perf-selfexcl). pg_sage's own
// sessions (application_name pg_sage) run its catalog reads, index builds
// and probes; the session inputs of the snapshot (system stats, locks,
// connection states and churn, the load circuit breaker) count the
// application's sessions only. The collector itself runs on a pool
// configured like pg_sage's, as in production, so its own connections are
// pg_sage's too.

// sageCollector is a collector on a pg_sage-configured pool.
func sageCollector(t *testing.T) (*Collector, *pgxpool.Pool) {
	t.Helper()
	testPool(t) // skips when the database is unavailable
	pool := selfload.SagePool(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return New(pool, testConfig(), version, noopLog), pool
}

func TestSystemStatsCountApplicationSessionsOnly(t *testing.T) {
	c, _ := sageCollector(t)
	selfload.Start(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
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
	_, app, sage := selfload.Start(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
	locks, err := c.collectLocks(context.Background())
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
	selfload.Start(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
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

// Churn is the application's new connections: starting pg_sage's sessions
// (its pools reconnect on their own schedule) does not move it.
func TestConnectionChurnIgnoresPgSageSessions(t *testing.T) {
	_, pool := sageCollector(t)
	w := selfload.New(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
	w.StartApp(t)
	before, err := collectConfigSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("collectConfigSnapshot: %v", err)
	}
	w.StartSage(t, 3)
	after, err := collectConfigSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("collectConfigSnapshot: %v", err)
	}
	if before.ConnectionChurn < 3 || after.ConnectionChurn != before.ConnectionChurn {
		t.Fatalf("churn %d -> %d after pg_sage opened 6 sessions, want unchanged (>= 3)",
			before.ConnectionChurn, after.ConnectionChurn)
	}
}

// The circuit breaker backs off when the server is busy; pg_sage's own
// active sessions are not load it should back off from. The ratio is
// cluster-wide (other test packages share the server), so it compares
// the minimum of several samples before and after pg_sage's sessions.
func TestCircuitBreakerLoadIgnoresPgSageSessions(t *testing.T) {
	_, pool := sageCollector(t)
	w := selfload.New(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
	w.StartApp(t)
	var maxConn float64
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('max_connections')::float8").Scan(&maxConn); err != nil {
		t.Fatalf("max_connections: %v", err)
	}
	active := func() (float64, error) {
		var ratio float64
		err := pool.QueryRow(context.Background(), loadRatioSQL).Scan(&ratio)
		return ratio * maxConn, err
	}
	before := selfload.MinOver(t, 8, active)
	w.StartSage(t, 6) // 8 active pg_sage sessions: waiter, sleeper, 6 more
	after := selfload.MinOver(t, 8, active)
	if after >= before+4 {
		t.Fatalf("active sessions in the load ratio %.0f -> %.0f after 8 active pg_sage "+
			"sessions started, want them left out", before, after)
	}
}
