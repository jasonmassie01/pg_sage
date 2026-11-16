package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// market-08 on a live server: workload_sessions groups this database's
// client sessions by (role, application, state), lists the long-running
// ones with their sqlcommenter tags, and never counts pg_sage's own.

// openSession opens one connection with application_name app.
func openSession(t *testing.T, pool *pgxpool.Pool, app string) *pgx.Conn {
	t.Helper()
	cfg := pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = app
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", app, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// sleepIn starts a tagged pg_sleep on conn and returns once it is active.
// A pgx.Conn is not safe for concurrent use, so the test's cleanup cancels
// the sleep and waits for it before openSession's cleanup closes conn
// (cleanups run last-registered first).
func sleepIn(t *testing.T, pool *pgxpool.Pool, conn *pgx.Conn, app, sql string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.Exec(ctx, sql)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND state = 'active'
			  AND query_start < now() - interval '1100 milliseconds'`, app).Scan(&n)
		if err == nil && n > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("session %s never became active for over a second", app)
}

func workloadStep(t *testing.T, c *Collector) catalogStep {
	t.Helper()
	for _, st := range c.extraSteps() {
		if st.name == "workload_sessions" {
			return st
		}
	}
	t.Fatal("no workload_sessions extra step")
	return catalogStep{}
}

func findGroup(ws *WorkloadSessions, app, state string) (WorkloadGroup, bool) {
	for _, g := range ws.Groups {
		if g.Application == app && g.State == state {
			return g, true
		}
	}
	return WorkloadGroup{}, false
}

// Happy path, self-exclusion and tags: one tagged active query, one idle
// in transaction session and one pg_sage session.
func TestWorkloadSessions_GroupsLongSessionsAndTags(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	active := openSession(t, pool, "wl_test_active")
	sleepIn(t, pool, active, "wl_test_active",
		"SELECT pg_sleep(6) /*route='%2Fcheckout',team='pay'*/")
	idle := openSession(t, pool, "wl_test_idle")
	if _, err := idle.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := idle.Exec(ctx, "SELECT 1 /*route='stale'*/"); err != nil {
		t.Fatalf("select in transaction: %v", err)
	}
	self := openSession(t, pool, "pg_sage")
	sleepIn(t, pool, self, "pg_sage", "SELECT pg_sleep(6)")
	time.Sleep(1100 * time.Millisecond) // the idle session's state age passes 1 s

	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap := &Snapshot{}
	if err := workloadStep(t, c).run(ctx, snap); err != nil {
		t.Fatalf("workload_sessions step: %v", err)
	}
	ws := snap.WorkloadSessions
	if ws == nil {
		t.Fatal("WorkloadSessions is nil after a successful read")
	}
	g, ok := findGroup(ws, "wl_test_active", "active")
	if !ok || g.Sessions != 1 || g.MaxQueryAgeSeconds < 1 || g.Role == "" {
		t.Fatalf("active group = %+v (found %v), want 1 session older than 1 s", g, ok)
	}
	ig, ok := findGroup(ws, "wl_test_idle", "idle in transaction")
	if !ok || ig.Sessions != 1 || ig.MaxStateAgeSeconds < 1 || ig.MaxXactAgeSeconds < 1 {
		t.Fatalf("idle in transaction group = %+v (found %v)", ig, ok)
	}
	if _, ok := findGroup(ws, "pg_sage", "active"); ok {
		t.Fatal("pg_sage's own session was grouped as workload")
	}
	assertLongSessions(t, ws)
}

func assertLongSessions(t *testing.T, ws *WorkloadSessions) {
	t.Helper()
	var sawActive, sawIdle bool
	for _, l := range ws.Long {
		switch l.Application {
		case "wl_test_active":
			sawActive = true
			if l.Tags["route"] != "/checkout" || l.Tags["team"] != "pay" || l.AgeSeconds < 1 {
				t.Errorf("active long session = %+v, want route and team tags", l)
			}
			if l.QueryStart.IsZero() || l.PID == 0 {
				t.Errorf("active long session lacks its identity: %+v", l)
			}
		case "wl_test_idle":
			sawIdle = true
			if len(l.Tags) != 0 || !strings.HasPrefix(l.State, "idle in transaction") {
				t.Errorf("idle long session = %+v, want no tags", l)
			}
		case "pg_sage":
			t.Errorf("pg_sage's own session listed as long-running: %+v", l)
		}
	}
	if !sawActive || !sawIdle {
		t.Fatalf("long sessions = %+v, want the active and the idle one", ws.Long)
	}
}

// Nil/empty: the category is read and available with non-nil slices,
// and the per-role cluster session counts include a second session of
// ours (the reading session itself is left out).
func TestWorkloadSessions_EmptyIsAvailable(t *testing.T) {
	pool := testPool(t)
	openSession(t, pool, "wl_test_other")
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !snap.Available("workload_sessions") || snap.WorkloadSessions == nil {
		t.Fatalf("workload_sessions available=%v value=%v, want an available reading",
			snap.Available("workload_sessions"), snap.WorkloadSessions)
	}
	if snap.WorkloadSessions.Groups == nil || snap.WorkloadSessions.Long == nil ||
		snap.WorkloadSessions.Roles == nil {
		t.Fatalf("empty reading has nil slices: %+v", snap.WorkloadSessions)
	}
	var me string
	if err := pool.QueryRow(context.Background(), "SELECT current_user").Scan(&me); err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.WorkloadSessions.Roles {
		if r.Role == me && r.ClusterSessions >= 1 {
			return
		}
	}
	t.Fatalf("roles = %+v, want %s with its cluster sessions", snap.WorkloadSessions.Roles, me)
}

// Per-role statement time comes from pg_stat_statements.userid; without
// the extension the role rows still carry their session counts.
func TestWorkloadSessions_RoleStatementTime(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	if !c.Capabilities().StatStatementsView {
		t.Skip("pg_stat_statements is not installed on this server")
	}
	// A statement shape no other test runs: pg_stat_statements normalizes
	// constants, so a bare "SELECT pg_sleep(0.2)" shares one entry with
	// every short pg_sleep other packages run on the same server, and that
	// entry's mean + 2.326 sd falls far below 200 ms.
	const slow = "SELECT count(*) AS workload_role_time" +
		" FROM pg_sleep(0.2), generate_series(1, 3)"
	if _, err := pool.Exec(ctx, slow); err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{}
	if err := workloadStep(t, c).run(ctx, snap); err != nil {
		t.Fatalf("workload_sessions step: %v", err)
	}
	var me string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&me); err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.WorkloadSessions.Roles {
		if r.Role != me {
			continue
		}
		if r.Calls < 1 || r.MaxExecMs < 190 || r.P99EstimateMs < 190 ||
			r.P99EstimateMs > r.MaxExecMs+0.001 {
			t.Fatalf("role stats = %+v, want the 200 ms statement", r)
		}
		return
	}
	t.Fatalf("no statement time for %s in %+v", me, snap.WorkloadSessions.Roles)
}

// Error propagation through collect: a failed read is unknown, not empty,
// and the rest of the snapshot is kept.
func TestWorkloadSessions_FailureIsUnknownInCollect(t *testing.T) {
	pool := testPool(t)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.overrideStep("workload_sessions", func(_ context.Context, s *Snapshot) error {
		s.WorkloadSessions = &WorkloadSessions{Groups: []WorkloadGroup{{Role: "partial"}}}
		return errors.New("permission denied for view pg_stat_activity")
	})
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.Available("workload_sessions") || snap.WorkloadSessions != nil {
		t.Fatalf("available=%v value=%+v, want unknown", snap.Available("workload_sessions"),
			snap.WorkloadSessions)
	}
	if !strings.Contains(snap.Unavailable["workload_sessions"], "permission denied") {
		t.Fatalf("Unavailable = %q", snap.Unavailable["workload_sessions"])
	}
	if !snap.Available("tables") {
		t.Fatalf("the rest of the snapshot was lost: %v", snap.Unavailable)
	}
}
