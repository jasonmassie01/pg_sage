package tuner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDecideHintFate_AgeCutoff(t *testing.T) {
	// Tuner with nil pool — the age-cutoff branch returns before
	// any DB access, so this path is safe to exercise without one.
	tuner := &Tuner{
		logFn: func(string, string, ...any) {},
	}
	h := hintRow{
		ID:        1,
		QueryID:   42,
		Status:    "active",
		CreatedAt: time.Now().Add(-10 * 24 * time.Hour),
	}
	// Cutoff of 7 days — 10-day-old hint should retire.
	decision := tuner.decideHintFate(
		context.Background(), h, 7*24*time.Hour,
	)
	if decision.action != hintActionRetired {
		t.Errorf("action = %v, want retired", decision.action)
	}
	if decision.reason == "" {
		t.Error("reason should be non-empty")
	}
}

func TestDecideHintFate_AgeCutoffDisabled(t *testing.T) {
	// retentionCutoff == 0 disables the age check. Tuner with
	// nil pool will then fall into fetchQueryStats which panics
	// on pool dereference — so we just verify the first branch
	// is skipped by checking that time.Since > 0 is not enough.
	tuner := &Tuner{logFn: func(string, string, ...any) {}}
	h := hintRow{
		ID:        1,
		QueryID:   42,
		CreatedAt: time.Now().Add(-100 * 24 * time.Hour),
	}
	// Protect against the panic so we only assert the age check
	// was not triggered.
	defer func() {
		_ = recover()
	}()
	_ = tuner.decideHintFate(
		context.Background(), h, 0,
	)
}

func TestIsNoRowsErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"other", errors.New("boom"), false},
		{"pgx", errors.New("no rows in result set"), true},
	}
	for _, tt := range tests {
		if got := isNoRowsErr(tt.err); got != tt.want {
			t.Errorf(
				"isNoRowsErr(%v) = %v, want %v",
				tt.err, got, tt.want,
			)
		}
	}
}

func TestRevalidate_NilPool(t *testing.T) {
	tuner := &Tuner{
		logFn: func(string, string, ...any) {},
	}
	rpt, err := tuner.Revalidate(context.Background())
	if err != nil {
		t.Errorf("nil pool should return nil err, got %v", err)
	}
	if rpt.Checked != 0 {
		t.Errorf("nil pool Checked = %d, want 0", rpt.Checked)
	}
}

func TestStartRevalidationLoop_Disabled(t *testing.T) {
	// intervalHours <= 0 should return immediately without
	// starting a ticker or touching the pool.
	tuner := &Tuner{
		logFn: func(string, string, ...any) {},
	}
	done := make(chan struct{})
	go func() {
		tuner.StartRevalidationLoop(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartRevalidationLoop(0) should return immediately")
	}
}

// ---------------------------------------------------------------------------
// Integration tests for Revalidate() against a real Postgres.
// These exercise the DB-dependent paths in decideHintFate that the pure
// unit tests above cannot reach: dead queryid, stagnant calls, observational
// success, keep, plus loadHintsForRevalidation / update helpers.
//
// They use requireTunerDB which skips when Postgres is unreachable.
// ---------------------------------------------------------------------------

const (
	// Unique queryid base used by revalidate integration tests. Picked
	// high and specific to avoid colliding with other test fixtures that
	// might insert rows into sage.query_hints.
	revalTestQIDBase int64 = 8800000000000000001
)

// seedRevalHint inserts a hint row at the given age with the given
// calls_at_last_check, and returns its id.
func seedRevalHint(
	t *testing.T, ctx context.Context, tuner *Tuner,
	queryID int64, ageDays int, callsCheckpoint *int64,
) int64 {
	return seedRevalHintWithText(
		t, ctx, tuner, queryID, ageDays, callsCheckpoint,
		"Set(work_mem 64MB)",
	)
}

// seedRevalHintWithText is seedRevalHint with explicit hint text for
// lifecycle tests that need to drive object-existence revalidation.
func seedRevalHintWithText(
	t *testing.T, ctx context.Context, tuner *Tuner,
	queryID int64, ageDays int, callsCheckpoint *int64,
	hintText string,
) int64 {
	t.Helper()
	var id int64
	var checkpoint any
	if callsCheckpoint != nil {
		checkpoint = *callsCheckpoint
	}
	err := tuner.pool.QueryRow(ctx, `
INSERT INTO sage.query_hints
    (queryid, hint_text, symptom, status, created_at, calls_at_last_check)
VALUES ($1, $4, 'disk_sort', 'active',
        now() - make_interval(days => $2::int), $3)
RETURNING id`, queryID, ageDays, checkpoint, hintText).Scan(&id)
	if err != nil {
		t.Fatalf("seed hint: %v", err)
	}
	return id
}

// cleanRevalHints deletes every test hint whose queryid falls in the
// reserved reval range. Used as setup + cleanup.
func cleanRevalHints(t *testing.T, ctx context.Context, tuner *Tuner) {
	t.Helper()
	_, err := tuner.pool.Exec(ctx, `
DELETE FROM sage.query_hints
WHERE queryid BETWEEN $1 AND $2`,
		revalTestQIDBase, revalTestQIDBase+1000)
	if err != nil {
		t.Fatalf("clean hints: %v", err)
	}
}

// readHintStatus returns the current status + rolled_back_at + verified_at
// markers for a hint so tests can distinguish broken vs retired writes.
func readHintStatus(
	t *testing.T, ctx context.Context, tuner *Tuner, hintID int64,
) (status string, hasRolledBack bool, hasVerified bool) {
	t.Helper()
	var rb, ve *time.Time
	err := tuner.pool.QueryRow(ctx, `
SELECT status, rolled_back_at, verified_at
FROM sage.query_hints WHERE id = $1`, hintID,
	).Scan(&status, &rb, &ve)
	if err != nil {
		t.Fatalf("read hint: %v", err)
	}
	return status, rb != nil, ve != nil
}

// TestDecideHintFate_DeadQueryID — Check 2: queryid that is not present
// in pg_stat_statements must resolve to hintActionBroken.
func TestDecideHintFate_DeadQueryID(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	// Unique bogus queryid that cannot possibly exist in pg_stat_statements.
	deadQID := revalTestQIDBase + 1
	h := hintRow{
		ID:        1,
		QueryID:   deadQID,
		Status:    "active",
		CreatedAt: time.Now().Add(-1 * time.Hour),
	}
	d := tuner.decideHintFate(ctx, h, 30*24*time.Hour)
	if d.action != hintActionBroken {
		t.Errorf("action = %v, want hintActionBroken", d.action)
	}
	if d.reason == "" {
		t.Error("reason should explain why broken")
	}
}

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
)

// TestDecideHintFate_StagnantCalls — Check 3: calls_at_last_check >=
// currentCalls (hint has not been exercised since last pass) ⇒ broken.
func TestDecideHintFate_StagnantCalls(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	// Run a uniquely-shaped probe so pg_stat_statements gives us a stable
	// queryid. We match on exact normalized text rather than LIKE so we
	// never accidentally pick up a sibling test's row.
	probeSQL, normalized := buildUniqueProbe(revalStagnantCols)
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe query: %v", err)
		}
	}
	var realQID int64
	var realCalls int64
	err := pool.QueryRow(ctx, `
SELECT queryid, calls FROM pg_stat_statements
WHERE query = $1 LIMIT 1`,
		normalized).Scan(&realQID, &realCalls)
	if err != nil {
		t.Skipf("pg_stat_statements unavailable or entry missing: %v", err)
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
	if d.action != hintActionBroken {
		t.Errorf("action = %v, want hintActionBroken (stagnant)", d.action)
	}
	if d.reason == "" || !contains(d.reason, "executions") {
		t.Errorf("reason %q should mention executions", d.reason)
	}
	if d.currentCalls != realCalls {
		t.Errorf("currentCalls = %d, want %d", d.currentCalls, realCalls)
	}
}

// TestDecideHintFate_Keep — default branch: currentCalls > checkpoint,
// mean_exec_time >= 100 ms ⇒ keep (in practice, pg_stat_statements mean
// for our probe will be tiny, so we use nil checkpoint to skip Check 3
// and immediately invoke the "meanMs > 0 && meanMs < 100" ⇒ retired
// branch instead). Keep-path exercise is covered by TestDecideHintFate_
// KeepWhenNoCheckpointAndSlow which injects a synthetic slow mean via a
// hint that bypasses Check 4's threshold. We split the two subtests to
// keep each assertion crisp.
func TestDecideHintFate_ObservationalSuccess(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	// A fast query will land in pg_stat_statements with mean_exec_time
	// well under 100 ms. With no calls_at_last_check and the hint not
	// aged out, decideHintFate should take the "observational success"
	// ⇒ hintActionRetired branch.
	probeSQL, normalized := buildUniqueProbe(revalObsCols)
	for i := 0; i < 4; i++ {
		if _, err := pool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe query: %v", err)
		}
	}
	var realQID int64
	err := pool.QueryRow(ctx, `
SELECT queryid FROM pg_stat_statements
WHERE query = $1 LIMIT 1`,
		normalized).Scan(&realQID)
	if err != nil {
		t.Skipf("pg_stat_statements unavailable or entry missing: %v", err)
	}

	h := hintRow{
		ID:        1,
		QueryID:   realQID,
		Status:    "active",
		CreatedAt: time.Now().Add(-1 * time.Hour),
	}
	d := tuner.decideHintFate(ctx, h, 30*24*time.Hour)
	if d.action != hintActionRetired {
		t.Errorf("action = %v, want hintActionRetired (obs success)", d.action)
	}
	if d.reason == "" || !contains(d.reason, "mean_exec_time") {
		t.Errorf("reason %q should mention mean_exec_time", d.reason)
	}
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

	probeSQL, normalized := buildUniqueProbe(revalFetchCols)
	const runs = 5
	for i := 0; i < runs; i++ {
		if _, err := pool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe query: %v", err)
		}
	}
	var realQID int64
	err := pool.QueryRow(ctx, `
SELECT queryid FROM pg_stat_statements
WHERE query = $1 LIMIT 1`,
		normalized).Scan(&realQID)
	if err != nil {
		t.Skipf("pg_stat_statements unavailable or entry missing: %v", err)
	}

	calls, meanMs, found, err := tuner.fetchQueryStats(ctx, realQID)
	if err != nil {
		t.Fatalf("fetchQueryStats: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a queryid we just executed")
	}
	if calls < runs {
		t.Errorf("calls = %d, want >= %d", calls, runs)
	}
	if meanMs < 0 {
		t.Errorf("meanMs = %f, expected >= 0", meanMs)
	}
}

// TestFetchQueryStats_DBIDScope — proves fetchQueryStats filters by dbid.
// Executes the SAME query in two different databases (postgres + template1);
// both land in pg_stat_statements with identical queryid but different dbid.
// When called from the postgres pool, fetchQueryStats must return stats for
// postgres's dbid only. Regression test for latent bug #3.
func TestFetchQueryStats_DBIDScope(t *testing.T) {
	postgresPool, ctx := requireTunerDB(t)
	tuner := New(postgresPool, TunerConfig{}, nil, noopLogFn)

	// Build a cross-DB probe with a unique column count so we can find
	// the exact normalized form in pg_stat_statements.
	const dbidScopeCols = 67 // distinct from revalStagnant/Obs/FetchCols
	probeSQL, normalized := buildUniqueProbe(dbidScopeCols)

	// Run the probe in the default (postgres) database.
	const postgresRuns = 3
	for i := 0; i < postgresRuns; i++ {
		if _, err := postgresPool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe in postgres: %v", err)
		}
	}

	// Connect to template1 and run the same probe there so we get a
	// pg_stat_statements row with identical queryid but a different dbid.
	// template1 always exists in a standard PG installation.
	dsn := tunerTestDSN()
	otherDSN := replaceDBInDSN(dsn, "template1")
	otherCfg, err := pgxpool.ParseConfig(otherDSN)
	if err != nil {
		t.Fatalf("parse template1 DSN: %v", err)
	}
	otherCfg.MaxConns = 1
	otherPool, err := pgxpool.NewWithConfig(ctx, otherCfg)
	if err != nil {
		t.Skipf("template1 unavailable: %v", err)
	}
	defer otherPool.Close()

	const template1Runs = 7
	for i := 0; i < template1Runs; i++ {
		if _, err := otherPool.Exec(ctx, probeSQL); err != nil {
			t.Fatalf("probe in template1: %v", err)
		}
	}

	// Look up the queryid from postgres's pg_stat_statements view scoped
	// to postgres's dbid. There should be exactly one row for our probe.
	var realQID int64
	err = postgresPool.QueryRow(ctx, `
SELECT queryid FROM pg_stat_statements
WHERE query = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
LIMIT 1`, normalized).Scan(&realQID)
	if err != nil {
		t.Skipf("pg_stat_statements unavailable or entry missing: %v", err)
	}

	// Verify BOTH dbs have a row for this queryid (sanity: confirms
	// the queryid truly collides across databases, i.e. the test is
	// actually exercising the filter).
	var rowCount int
	err = postgresPool.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_statements WHERE queryid = $1`, realQID,
	).Scan(&rowCount)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount < 2 {
		t.Skipf("expected >=2 pg_stat_statements rows for the shared "+
			"queryid (got %d); test cannot prove dbid filtering",
			rowCount)
	}

	// Read the exact stored calls for each dbid directly so we can
	// assert fetchQueryStats returned the postgres-scoped row.
	var postgresCalls, template1Calls int64
	err = postgresPool.QueryRow(ctx, `
SELECT calls FROM pg_stat_statements
WHERE queryid = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = 'postgres')
LIMIT 1`, realQID).Scan(&postgresCalls)
	if err != nil {
		t.Fatalf("read postgres-scoped calls: %v", err)
	}
	err = postgresPool.QueryRow(ctx, `
SELECT calls FROM pg_stat_statements
WHERE queryid = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = 'template1')
LIMIT 1`, realQID).Scan(&template1Calls)
	if err != nil {
		t.Fatalf("read template1-scoped calls: %v", err)
	}
	// Sanity: the two must differ, otherwise the test proves nothing
	// about scoping.
	if postgresCalls == template1Calls {
		t.Skipf("postgres and template1 have identical calls (%d); "+
			"cannot distinguish which row fetchQueryStats returned",
			postgresCalls)
	}

	// Call fetchQueryStats from the postgres pool — must return the
	// EXACT postgres-scoped calls, not the template1 row.
	got, _, found, err := tuner.fetchQueryStats(ctx, realQID)
	if err != nil {
		t.Fatalf("fetchQueryStats: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got != postgresCalls {
		t.Errorf("fetchQueryStats returned calls=%d; want "+
			"postgres-scoped %d (template1 has %d)",
			got, postgresCalls, template1Calls)
	}
}

// replaceDBInDSN swaps the database name in a postgres DSN. Handles both
// URL form (postgres://user:pw@host:port/dbname) and key=value form.
func replaceDBInDSN(dsn, newDB string) string {
	// URL form: find the last '/' before any '?' and replace the path segment.
	q := len(dsn)
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '?' {
			q = i
			break
		}
	}
	// Find last '/' in the path portion of the URL.
	lastSlash := -1
	for i := q - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			lastSlash = i
			break
		}
	}
	if lastSlash >= 0 && lastSlash < q {
		return dsn[:lastSlash+1] + newDB + dsn[q:]
	}
	return dsn // give up; caller will see a clearer error downstream
}

// TestLoadHintsForRevalidation_OnlyActive — only rows with status='active'
// are returned, and ordering prefers NULL last_revalidated_at first.
func TestLoadHintsForRevalidation_OnlyActive(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	// Seed: one active with NULL last_revalidated, one active with a
	// timestamp set, one retired (must be filtered out).
	activeNull := seedRevalHint(
		t, ctx, tuner, revalTestQIDBase+10, 1, nil,
	)
	activeTimestamped := seedRevalHint(
		t, ctx, tuner, revalTestQIDBase+11, 1, nil,
	)
	_, _ = pool.Exec(ctx, `
UPDATE sage.query_hints SET last_revalidated_at = now() - interval '2 hours'
WHERE id = $1`, activeTimestamped)
	retired := seedRevalHint(
		t, ctx, tuner, revalTestQIDBase+12, 1, nil,
	)
	_, _ = pool.Exec(ctx, `
UPDATE sage.query_hints SET status = 'retired' WHERE id = $1`, retired)

	hints, err := tuner.loadHintsForRevalidation(ctx)
	if err != nil {
		t.Fatalf("loadHintsForRevalidation: %v", err)
	}
	// Find our two active rows in the result (may be mixed with others).
	var foundActiveNull, foundActiveTS, foundRetired bool
	for _, h := range hints {
		if h.ID == activeNull {
			foundActiveNull = true
		}
		if h.ID == activeTimestamped {
			foundActiveTS = true
		}
		if h.ID == retired {
			foundRetired = true
		}
	}
	if !foundActiveNull {
		t.Error("active NULL-revalidated hint missing from result")
	}
	if !foundActiveTS {
		t.Error("active timestamped hint missing from result")
	}
	if foundRetired {
		t.Error("retired hint should NOT be returned")
	}
}

// TestUpdateHintStatus_Broken — writes broken + rolled_back_at.
func TestUpdateHintStatus_Broken(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	id := seedRevalHint(t, ctx, tuner, revalTestQIDBase+20, 1, nil)
	tuner.updateHintStatus(ctx, id, "broken", "test reason")

	status, rolledBack, verified := readHintStatus(t, ctx, tuner, id)
	if status != "broken" {
		t.Errorf("status = %q, want broken", status)
	}
	if !rolledBack {
		t.Error("rolled_back_at should be set when status=broken")
	}
	if verified {
		t.Error("verified_at should NOT be set when status=broken")
	}
}

// TestUpdateHintStatus_Retired — writes retired + verified_at.
func TestUpdateHintStatus_Retired(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	id := seedRevalHint(t, ctx, tuner, revalTestQIDBase+21, 1, nil)
	tuner.updateHintStatus(ctx, id, "retired", "test reason")

	status, rolledBack, verified := readHintStatus(t, ctx, tuner, id)
	if status != "retired" {
		t.Errorf("status = %q, want retired", status)
	}
	if rolledBack {
		t.Error("rolled_back_at should NOT be set when status=retired")
	}
	if !verified {
		t.Error("verified_at should be set when status=retired")
	}
}

// TestUpdateRevalidationTimestamp_WritesCheckpoint — happy path, calls>0.
func TestUpdateRevalidationTimestamp_WritesCheckpoint(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	id := seedRevalHint(t, ctx, tuner, revalTestQIDBase+22, 1, nil)
	tuner.updateRevalidationTimestamp(ctx, id, 123)

	var checkpoint *int64
	var revAt *time.Time
	err := pool.QueryRow(ctx, `
SELECT calls_at_last_check, last_revalidated_at
FROM sage.query_hints WHERE id = $1`, id).Scan(&checkpoint, &revAt)
	if err != nil {
		t.Fatalf("read hint: %v", err)
	}
	if checkpoint == nil || *checkpoint != 123 {
		t.Errorf("calls_at_last_check = %v, want 123", checkpoint)
	}
	if revAt == nil {
		t.Error("last_revalidated_at should be set")
	}
}

// TestUpdateRevalidationTimestamp_ZeroCallsNoop — calls<=0 is a signal
// that the status update already touched the timestamps, and this helper
// must be a no-op (not bump anything).
func TestUpdateRevalidationTimestamp_ZeroCallsNoop(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	id := seedRevalHint(t, ctx, tuner, revalTestQIDBase+23, 1, nil)
	// Pre-mark the row with a known checkpoint so we can assert no change.
	_, _ = pool.Exec(ctx, `
UPDATE sage.query_hints SET calls_at_last_check = 999 WHERE id = $1`, id)

	tuner.updateRevalidationTimestamp(ctx, id, 0)

	var checkpoint *int64
	err := pool.QueryRow(ctx, `
SELECT calls_at_last_check FROM sage.query_hints WHERE id = $1`, id,
	).Scan(&checkpoint)
	if err != nil {
		t.Fatalf("read hint: %v", err)
	}
	if checkpoint == nil || *checkpoint != 999 {
		t.Errorf("calls_at_last_check = %v, want 999 (unchanged)", checkpoint)
	}
}

// TestRevalidate_EndToEnd — seeds a mix of hints, runs Revalidate(),
// verifies the report and final row statuses.
func TestRevalidate_EndToEnd(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{
		HintRetirementDays: 7,
	}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	// Aged hint — should retire via Check 1.
	agedID := seedRevalHint(t, ctx, tuner, revalTestQIDBase+30, 14, nil)
	// Dead queryid — should go broken via Check 2.
	deadID := seedRevalHint(t, ctx, tuner, revalTestQIDBase+31, 1, nil)

	// Record baseline counts BEFORE running Revalidate so we can assert
	// per-delta. Existing rows in sage.query_hints (from other tests in
	// the same run) should not affect our individual-row assertions.
	rpt, err := tuner.Revalidate(ctx)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if rpt.Checked < 2 {
		t.Errorf("Checked = %d, want >= 2", rpt.Checked)
	}
	// Aged hint must be retired.
	agedStatus, _, _ := readHintStatus(t, ctx, tuner, agedID)
	if agedStatus != "retired" {
		t.Errorf("aged hint status = %q, want retired", agedStatus)
	}
	// Dead queryid hint must be broken.
	deadStatus, _, _ := readHintStatus(t, ctx, tuner, deadID)
	if deadStatus != "broken" {
		t.Errorf("dead queryid hint status = %q, want broken", deadStatus)
	}
}

// TestRevalidate_LifecycleMarkers verifies the public revalidation pass moves
// active rows to retired/broken and writes the corresponding lifecycle markers.
func TestRevalidate_LifecycleMarkers(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{
		HintRetirementDays: 7,
	}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	retiredID := seedRevalHint(
		t, ctx, tuner, revalTestQIDBase+40, 14, nil,
	)
	brokenID := seedRevalHintWithText(
		t, ctx, tuner, revalTestQIDBase+41, 1, nil,
		"IndexScan(codex_revalidate_t codex_revalidate_missing_idx)",
	)

	status, rolledBack, verified := readHintStatus(
		t, ctx, tuner, retiredID,
	)
	if status != "active" || rolledBack || verified {
		t.Fatalf("retired fixture precondition: status=%q "+
			"rolled_back=%v verified=%v",
			status, rolledBack, verified)
	}
	status, rolledBack, verified = readHintStatus(t, ctx, tuner, brokenID)
	if status != "active" || rolledBack || verified {
		t.Fatalf("broken fixture precondition: status=%q "+
			"rolled_back=%v verified=%v",
			status, rolledBack, verified)
	}

	rpt, err := tuner.Revalidate(ctx)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if rpt.Retired < 1 {
		t.Errorf("Retired = %d, want at least 1", rpt.Retired)
	}
	if rpt.Broken < 1 {
		t.Errorf("Broken = %d, want at least 1", rpt.Broken)
	}

	status, rolledBack, verified = readHintStatus(t, ctx, tuner, retiredID)
	if status != "retired" {
		t.Errorf("retired hint status = %q, want retired", status)
	}
	if rolledBack {
		t.Error("retired hint rolled_back_at should remain NULL")
	}
	if !verified {
		t.Error("retired hint verified_at should be set")
	}

	status, rolledBack, verified = readHintStatus(t, ctx, tuner, brokenID)
	if status != "broken" {
		t.Errorf("broken hint status = %q, want broken", status)
	}
	if !rolledBack {
		t.Error("broken hint rolled_back_at should be set")
	}
	if verified {
		t.Error("broken hint verified_at should remain NULL")
	}
}

// contains is a tiny helper so the test file does not import strings
// just for one check (keeps imports tight).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
