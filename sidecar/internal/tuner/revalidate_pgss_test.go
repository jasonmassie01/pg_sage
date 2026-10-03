package tuner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// Revalidation tests that read a probe's statistics from
// pg_stat_statements. The view is shared by the whole server: another test
// package's pg_stat_statements_reset() (or an eviction) can erase the
// probe between running it and reading it, so each test repeats its
// measurement then (pgssepoch) instead of failing or skipping.

// buildUniqueProbe returns (probeSQL, normalized) for a SELECT with exactly
// colCount integer literals. pg_stat_statements hashes queries by their
// normalized form (literals replaced with $N), so each test uses a DIFFERENT
// colCount to produce a distinct queryid. Column aliases do NOT affect the
// queryid hash, so we rely on column count for uniqueness.
func buildUniqueProbe(colCount int) (probeSQL, normalized string) {
	probeSQL = "SELECT 1"
	normalized = "SELECT $1"
	for i := 2; i <= colCount; i++ {
		probeSQL += fmt.Sprintf(", %d", i)
		normalized += fmt.Sprintf(", $%d", i)
	}
	return probeSQL, normalized
}

// Reserved column counts for probe-based revalidate tests. Each must be
// unique within this package so pg_stat_statements returns distinct rows.
const (
	revalStagnantCols = 47
	revalObsCols      = 53
	revalFetchCols    = 59
	revalDBIDCols     = 67
)

// runRevalProbe runs the probe with colCount columns runs times on pool and
// reads its queryid and calls in the pool's database. A missing entry is
// returned as a problem.
func runRevalProbe(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	colCount, runs int) (qid, calls int64, problem string) {
	t.Helper()
	probeSQL, normalized := buildUniqueProbe(colCount)
	for i := 0; i < runs; i++ {
		if _, err := pool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe query: %v", err)
		}
	}
	err := pool.QueryRow(ctx, `
SELECT queryid, calls FROM pg_stat_statements
WHERE query = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
LIMIT 1`,
		normalized).Scan(&qid, &calls)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Sprintf("probe %q missing from pg_stat_statements", normalized)
	}
	if err != nil {
		t.Fatalf("read probe from pg_stat_statements: %v", err)
	}
	return qid, calls, ""
}

// TestDecideHintFate_StagnantCalls — Check 3: calls_at_last_check >=
// currentCalls (hint has not been exercised since last pass) ⇒ broken.
func TestDecideHintFate_StagnantCalls(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	// Run a uniquely-shaped probe so pg_stat_statements gives us a stable
	// queryid. We match on exact normalized text rather than LIKE so we
	// never accidentally pick up a sibling test's row.
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		realQID, realCalls, missing := runRevalProbe(t, ctx, pool, revalStagnantCols, 3)
		if missing != "" {
			return []string{missing}
		}
		// Set calls_at_last_check to realCalls so currentCalls <= checkpoint.
		checkpoint := realCalls
		h := hintRow{
			ID:               1,
			QueryID:          realQID,
			Status:           "active",
			CreatedAt:        time.Now().Add(-1 * time.Hour),
			CallsAtLastCheck: &checkpoint,
		}
		d := tuner.decideHintFate(ctx, h, 30*24*time.Hour)
		var problems []string
		if d.action != hintActionBroken {
			problems = append(problems, fmt.Sprintf(
				"action = %v, want hintActionBroken (stagnant)", d.action))
		}
		if d.reason == "" || !contains(d.reason, "executions") {
			problems = append(problems, fmt.Sprintf(
				"reason %q should mention executions", d.reason))
		}
		if d.currentCalls != realCalls {
			problems = append(problems, fmt.Sprintf(
				"currentCalls = %d, want %d", d.currentCalls, realCalls))
		}
		return problems
	})
}

// TestDecideHintFate_ObservationalSuccess — a fast query lands in
// pg_stat_statements with mean_exec_time well under 100 ms. With no
// calls_at_last_check and the hint not aged out, decideHintFate takes the
// "observational success" ⇒ hintActionRetired branch.
func TestDecideHintFate_ObservationalSuccess(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		realQID, _, missing := runRevalProbe(t, ctx, pool, revalObsCols, 4)
		if missing != "" {
			return []string{missing}
		}
		h := hintRow{
			ID:        1,
			QueryID:   realQID,
			Status:    "active",
			CreatedAt: time.Now().Add(-1 * time.Hour),
		}
		d := tuner.decideHintFate(ctx, h, 30*24*time.Hour)
		var problems []string
		if d.action != hintActionRetired {
			problems = append(problems, fmt.Sprintf(
				"action = %v, want hintActionRetired (obs success)", d.action))
		}
		if d.reason == "" || !contains(d.reason, "mean_exec_time") {
			problems = append(problems, fmt.Sprintf(
				"reason %q should mention mean_exec_time", d.reason))
		}
		return problems
	})
}

// TestFetchQueryStats_NotFound — absent queryid returns found=false
// with err=nil, rather than a raw ErrNoRows.
func TestFetchQueryStats_NotFound(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	calls, meanMs, found, err := tuner.fetchQueryStats(
		ctx, revalTestQIDBase+777,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("found should be false for absent queryid")
	}
	if calls != 0 || meanMs != 0 {
		t.Errorf("expected zero stats, got calls=%d mean=%f", calls, meanMs)
	}
}

// TestFetchQueryStats_Found — run a query, look up its queryid, fetch
// its stats via fetchQueryStats, and verify the returned calls value
// is at least the number we issued.
func TestFetchQueryStats_Found(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	const runs = 5
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		realQID, _, missing := runRevalProbe(t, ctx, pool, revalFetchCols, runs)
		if missing != "" {
			return []string{missing}
		}
		calls, meanMs, found, err := tuner.fetchQueryStats(ctx, realQID)
		if err != nil {
			t.Fatalf("fetchQueryStats: %v", err)
		}
		if !found {
			return []string{"expected found=true for a queryid we just executed"}
		}
		if calls < runs {
			return []string{fmt.Sprintf("calls = %d, want >= %d", calls, runs)}
		}
		if meanMs < 0 {
			t.Errorf("meanMs = %f, expected >= 0", meanMs)
		}
		return nil
	})
}

// TestFetchQueryStats_DBIDScope — proves fetchQueryStats filters by dbid.
// Executes the SAME query in two different databases (the test database
// and template1); both land in pg_stat_statements with identical queryid
// but different dbid. When called from the test database's pool,
// fetchQueryStats must return stats for that dbid only. Regression test
// for latent bug #3.
func TestFetchQueryStats_DBIDScope(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)
	other := template1Pool(t, ctx)
	probeSQL, _ := buildUniqueProbe(revalDBIDCols)

	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		// Unequal run counts, so the two rows' calls tell them apart.
		for i := 0; i < 7; i++ {
			if _, err := other.Exec(ctx, probeSQL); err != nil {
				t.Fatalf("probe in template1: %v", err)
			}
		}
		realQID, ownCalls, missing := runRevalProbe(t, ctx, pool, revalDBIDCols, 3)
		if missing != "" {
			return []string{missing}
		}
		// The queryid must truly collide across databases, and the rows
		// must differ, or the test proves nothing about scoping.
		otherCalls, found := callsInDatabase(t, ctx, pool, realQID, "template1")
		if !found || otherCalls == ownCalls {
			return []string{fmt.Sprintf("template1 row for queryid %d: found=%v "+
				"calls=%d (test database %d); cannot prove dbid filtering",
				realQID, found, otherCalls, ownCalls)}
		}
		got, _, found, err := tuner.fetchQueryStats(ctx, realQID)
		if err != nil {
			t.Fatalf("fetchQueryStats: %v", err)
		}
		if !found || got != ownCalls {
			return []string{fmt.Sprintf("fetchQueryStats returned calls=%d found=%v; "+
				"want the test database's %d (template1 has %d)",
				got, found, ownCalls, otherCalls)}
		}
		return nil
	})
}

// template1Pool connects to template1 on the test server (closed when the
// test ends); the test is skipped when it cannot.
func template1Pool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(replaceDBInDSN(tunerTestDSN(), "template1"))
	if err != nil {
		t.Fatalf("parse template1 DSN: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("template1 unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// callsInDatabase reads the calls of queryid qid in database db.
func callsInDatabase(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	qid int64, db string) (int64, bool) {
	t.Helper()
	var calls int64
	err := pool.QueryRow(ctx, `
SELECT calls FROM pg_stat_statements
WHERE queryid = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = $2)
LIMIT 1`, qid, db).Scan(&calls)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read %s-scoped calls: %v", db, err)
	}
	return calls, true
}
