package perfgate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The plan tests built on AnalyzeSage assume what a long-running
// deployment has: history autovacuum has marked all-visible, so index-only
// scans read no heap. VACUUM can mark a page only once no snapshot in the
// database can still see its rows as in progress. An autovacuum worker
// that began analyzing a large seeded table before the last seed insert
// committed holds such a snapshot until it finishes; VACUUM then left the
// newest table unmarked, and the planner read sage.shadow_decision whole
// instead of its covering index (CI, PG17: "7 summaries scanned
// sage.shadow_decision 7 times").
//
// holdSnapshot stands in for that worker: a REPEATABLE READ snapshot in
// the database, taken before the rows are written.
func holdSnapshot(t *testing.T, ctx context.Context, dsn string) func() {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect snapshot holder: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	for _, sql := range []string{"BEGIN ISOLATION LEVEL REPEATABLE READ", "SELECT 1"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return func() {
		if _, err := conn.Exec(context.Background(), "COMMIT"); err != nil {
			t.Errorf("end the held snapshot: %v", err)
		}
	}
}

func seedVisibilityProbe(t *testing.T, ctx context.Context, q Querier) {
	t.Helper()
	for _, sql := range []string{"CREATE SCHEMA IF NOT EXISTS sage",
		"CREATE TABLE sage.visibility_probe (id int, pad text) WITH (autovacuum_enabled = off)",
		"INSERT INTO sage.visibility_probe SELECT g, repeat('x', 200) FROM " +
			"generate_series(1, 5000) g"} {
		if _, err := q.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func probeVisibility(t *testing.T, ctx context.Context, q Querier) (pages, visible int64) {
	t.Helper()
	if err := q.QueryRow(ctx, `SELECT relpages, relallvisible FROM pg_catalog.pg_class
		WHERE oid = 'sage.visibility_probe'::regclass`).Scan(&pages, &visible); err != nil {
		t.Fatalf("read visibility of sage.visibility_probe: %v", err)
	}
	return pages, visible
}

// AnalyzeSage waits out a snapshot older than the seeded rows and returns
// with every page of the sage tables all-visible.
func TestAnalyzeSageLeavesTheHistoryAllVisible(t *testing.T) {
	pool, ctx := livePool(t)
	release := holdSnapshot(t, ctx, pool.Config().ConnString())
	seedVisibilityProbe(t, ctx, pool)
	done := make(chan error, 1)
	go func() { done <- AnalyzeSage(ctx, pool) }()
	select {
	case err := <-done:
		t.Fatalf("AnalyzeSage returned (%v) while a snapshot older than the rows was "+
			"held: the rows cannot be all-visible yet", err)
	case <-time.After(2 * time.Second):
	}
	if pages, visible := probeVisibility(t, ctx, pool); pages == 0 || visible != 0 {
		t.Fatalf("relpages %d relallvisible %d under the held snapshot, want no page "+
			"marked (the test holds nothing back otherwise)", pages, visible)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AnalyzeSage: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("AnalyzeSage did not return after the snapshot ended")
	}
	if pages, visible := probeVisibility(t, ctx, pool); pages == 0 || visible != pages {
		t.Fatalf("relpages %d relallvisible %d, want every page all-visible", pages, visible)
	}
}

// A snapshot that outlasts the wait is an error naming the table, not a
// silently half-vacuumed database.
func TestAwaitAllVisibleNamesWhatStaysUnmarked(t *testing.T) {
	pool, ctx := livePool(t)
	holdSnapshot(t, ctx, pool.Config().ConnString())
	seedVisibilityProbe(t, ctx, pool)
	if _, err := pool.Exec(ctx, "VACUUM sage.visibility_probe"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := awaitAllVisible(ctx, pool, time.Second)
	if err == nil || !strings.Contains(err.Error(), "sage.visibility_probe") {
		t.Fatalf("err = %v, want one naming sage.visibility_probe", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("gave up after %v, want about the 1 s given", took)
	}
}

// Nothing to wait for: an empty database, and one already all-visible.
func TestAwaitAllVisibleReturnsAtOnceWhenNothingIsLeft(t *testing.T) {
	pool, ctx := livePool(t)
	if err := awaitAllVisible(ctx, pool, time.Second); err != nil {
		t.Fatalf("no sage schema: %v", err)
	}
	seedVisibilityProbe(t, ctx, pool)
	// One VACUUM is not enough on a shared server: the previous test's
	// snapshot holder, closed by the client, can still be ending on the
	// server (PG14 on CI), and an autovacuum worker can hold one too.
	if err := awaitAllVisible(ctx, pool, time.Minute); err != nil {
		t.Fatalf("probe never became all-visible: %v", err)
	}
	if err := awaitAllVisible(ctx, pool, 0); err != nil {
		t.Fatalf("all-visible table, no time to wait: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := awaitAllVisible(cancelled, pool, time.Second); err == nil {
		t.Fatal("cancelled context: want its error")
	}
}
