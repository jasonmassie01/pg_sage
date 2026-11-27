//go:build integration

package advisor

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
)

// advisors-07 on real PostgreSQL: the history read gives the window's
// peak of total backends, and the sessions counter sees connections that
// already closed, which the old live-session sample never did.

// oldLiveSessionSample is the deleted connectionChurnSQL: sessions still
// connected that started in the last 5 minutes. It is narrowed to the
// test's own sessions ($1, their application_name): other tests' pools
// open connections at any time, which made a whole-database count flaky.
const oldLiveSessionSample = `SELECT count(*)::int FROM pg_stat_activity
 WHERE backend_start > now() - interval '5 minutes'
   AND backend_type = 'client backend' AND datname = current_database()
   AND application_name = $1`

// churnTestApp names the connect-and-close sessions.
const churnTestApp = "advisors07_churn_test"

type sessionCounter struct {
	sessions int64
	epoch    time.Time
}

func readSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) sessionCounter {
	t.Helper()
	var c sessionCounter
	err := pool.QueryRow(ctx, `SELECT sessions,
		GREATEST(COALESCE(stats_reset, '-infinity'::timestamptz), pg_postmaster_start_time())
		FROM pg_stat_database WHERE datname = current_database()`).Scan(&c.sessions, &c.epoch)
	if err != nil {
		t.Fatalf("read pg_stat_database.sessions: %v", err)
	}
	c.epoch = c.epoch.UTC()
	return c
}

// insertSystemRow writes a system snapshot row the way the collector does
// (the SystemStats JSON), age ago.
func insertSystemRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sys collector.SystemStats, age time.Duration) {
	t.Helper()
	data, err := json.Marshal(sys)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	err = pool.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - make_interval(secs => $1), 'system', $2) RETURNING id`,
		age.Seconds(), data).Scan(&id)
	if err != nil {
		t.Fatalf("insert system snapshot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.snapshots WHERE id = $1`, id)
	})
}

func connectAndClose(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse SAGE_DATABASE_URL: %v", err)
	}
	cfg.RuntimeParams["application_name"] = churnTestApp
	for i := 0; i < n; i++ {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		if err := conn.Close(ctx); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

func TestConnectionHistory_LivePG_PeakAndChurn(t *testing.T) {
	pool := runsPool(t)
	ctx := context.Background()
	clearSystemRows(t, ctx, pool)
	before := readSessions(t, ctx, pool)
	insertSystemRow(t, ctx, pool, collector.SystemStats{TotalBackends: 400,
		Sessions: before.sessions, RelationStatsEpoch: before.epoch}, 2*time.Minute)
	insertSystemRow(t, ctx, pool, collector.SystemStats{TotalBackends: 50,
		Sessions: before.sessions, RelationStatsEpoch: before.epoch}, time.Minute)
	// Outside the window: a higher peak that must not count.
	insertSystemRow(t, ctx, pool, collector.SystemStats{TotalBackends: 900}, 3*time.Hour)

	var oldBefore int
	if err := pool.QueryRow(ctx, oldLiveSessionSample, churnTestApp).Scan(
		&oldBefore); err != nil {
		t.Fatal(err)
	}
	connectAndClose(t, ctx, 100)
	// Wait for the counter first: by then the closed backends have exited,
	// so none lingers in pg_stat_activity for the old sample.
	after := waitSessions(t, ctx, pool, before.sessions+100)
	var oldAfter int
	if err := pool.QueryRow(ctx, oldLiveSessionSample, churnTestApp).Scan(
		&oldAfter); err != nil {
		t.Fatal(err)
	}

	w, err := pgConnHistory{db: pool}.readConnWindow(ctx, time.Hour)
	if err != nil {
		t.Fatalf("readConnWindow: %v", err)
	}
	if !w.peakKnown || w.peakTotal != 400 {
		t.Errorf("peak = %d (known %v), want 400 from the window only", w.peakTotal,
			w.peakKnown)
	}
	snap := &collector.Snapshot{CollectedAt: time.Now(),
		System: collector.SystemStats{Sessions: after.sessions,
			RelationStatsEpoch: after.epoch}}
	opened, perMin, ok := connChurn(snap, w)
	if !ok || opened < 100 || perMin <= 0 {
		t.Errorf("churn = %d (%.1f/min, known %v), want at least 100 opened", opened,
			perMin, ok)
	}
	if oldBefore != 0 || oldAfter != 0 {
		t.Errorf("old live-session sample moved %d -> %d; it never saw closed sessions",
			oldBefore, oldAfter)
	}
	t.Logf("sessions opened %d (%.1f/min); old sample %d -> %d", opened, perMin,
		oldBefore, oldAfter)
}

// No history at all is unknown, not a peak of 0.
func TestConnectionHistory_LivePG_EmptyIsUnknown(t *testing.T) {
	pool := runsPool(t)
	ctx := context.Background()
	clearSystemRows(t, ctx, pool)
	w, err := pgConnHistory{db: pool}.readConnWindow(ctx, time.Hour)
	if err != nil {
		t.Fatalf("readConnWindow: %v", err)
	}
	if w.peakKnown || w.sessionsKnown {
		t.Errorf("empty history: %+v, want unknown", w)
	}
}

func clearSystemRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`DELETE FROM sage.snapshots WHERE category = 'system'`); err != nil {
		t.Fatalf("clear system snapshots: %v", err)
	}
}

// waitSessions waits (bounded) for the statistics to count the closed
// sessions: PG14's collector is asynchronous, PG15+ flushes at exit.
func waitSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	want int64) sessionCounter {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c := readSessions(t, ctx, pool)
		if c.sessions >= want || time.Now().After(deadline) {
			return c
		}
		time.Sleep(200 * time.Millisecond)
	}
}
