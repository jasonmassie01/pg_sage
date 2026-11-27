package tuner

import (
	"context"
	"testing"
	"time"
)

// Without a pool there are no statistics to read: the decision is an
// error and the hint keeps its status (tuning-01). The age rule no longer
// short-circuits before the statistics read, because age only marks a hint
// due for re-verification (TestDecideFromObservation_AgeMarksReverifyDue).
func TestDecideHintFate_NilPoolIsError(t *testing.T) {
	tuner := &Tuner{logFn: func(string, string, ...any) {}}
	h := hintRow{
		ID:        1,
		QueryID:   42,
		Status:    "active",
		CreatedAt: time.Now().Add(-100 * 24 * time.Hour),
	}
	d := tuner.decideHintFate(context.Background(), h, 7*24*time.Hour)
	if d.action != hintActionError {
		t.Fatalf("action = %v, want hintActionError", d.action)
	}
	if d.nextStatus(h) != "active" {
		t.Fatalf("next status = %q, want active (unchanged)", d.nextStatus(h))
	}
	if !contains(d.reason, "pool") {
		t.Fatalf("reason %q should name the missing pool", d.reason)
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
// unit tests cannot reach: an absent queryid, no new calls, a fast hinted
// query, plus loadHintsForRevalidation and the recordRevalidation write.
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

// TestDecideHintFate_DeadQueryID — a queryid that is not present in
// pg_stat_statements (an eviction, or a query that stopped running) is
// dormant: the hint stays installed. It used to be marked broken (tuning-01).
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
	if d.action != hintActionDormant {
		t.Errorf("action = %v, want hintActionDormant", d.action)
	}
	if !contains(d.reason, "absent") {
		t.Errorf("reason %q should say the queryid is absent", d.reason)
	}
	if d.checkpoint != nil {
		t.Errorf("checkpoint = %d, want unchanged (nil)", *d.checkpoint)
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

// TestRevalidate_EndToEnd — seeds a mix of hints, runs Revalidate(),
// verifies the report and final row statuses.
func TestRevalidate_EndToEnd(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{
		HintRetirementDays: 7,
	}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	// Aged hint: kept and stamped due for re-verification (age_reverify).
	agedID := seedRevalHint(t, ctx, tuner, revalTestQIDBase+30, 14, nil)
	// Dead queryid: dormant, still installed.
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
	// The aged hint's queryid is absent too, so it is dormant, and due.
	agedStatus, _, verified := readHintStatus(t, ctx, tuner, agedID)
	if agedStatus != "dormant" || verified {
		t.Errorf("aged hint status = %q verified=%v, want dormant, unverified",
			agedStatus, verified)
	}
	if !hintReverifyDue(t, ctx, pool, agedID) {
		t.Error("aged hint should be stamped reverify_due")
	}
	if hintReverifyDue(t, ctx, pool, deadID) {
		t.Error("one-day-old hint must not be due for re-verification")
	}
	deadStatus, rolledBack, _ := readHintStatus(t, ctx, tuner, deadID)
	if deadStatus != "dormant" || rolledBack {
		t.Errorf("dead queryid hint status = %q rolled_back=%v, want dormant",
			deadStatus, rolledBack)
	}
	if rpt.Dormant < 2 || rpt.ReverifyDue < 1 {
		t.Errorf("report = %+v, want >=2 dormant and >=1 reverify due", rpt)
	}
}

// TestRevalidate_LifecycleMarkers verifies the public revalidation pass: a
// missing referenced index is broken (with rolled_back_at), and an aged hint
// is no longer retired but kept and stamped reverify_due (tuning-01).
func TestRevalidate_LifecycleMarkers(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tuner := New(pool, TunerConfig{
		HintRetirementDays: 7,
	}, nil, noopLogFn)

	cleanRevalHints(t, ctx, tuner)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tuner) })

	agedID := seedRevalHint(
		t, ctx, tuner, revalTestQIDBase+40, 14, nil,
	)
	brokenID := seedRevalHintWithText(
		t, ctx, tuner, revalTestQIDBase+41, 1, nil,
		"IndexScan(codex_revalidate_t codex_revalidate_missing_idx)",
	)

	status, rolledBack, verified := readHintStatus(
		t, ctx, tuner, agedID,
	)
	if status != "active" || rolledBack || verified {
		t.Fatalf("aged fixture precondition: status=%q "+
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
	if rpt.ReverifyDue < 1 {
		t.Errorf("ReverifyDue = %d, want at least 1", rpt.ReverifyDue)
	}
	if rpt.Broken < 1 {
		t.Errorf("Broken = %d, want at least 1", rpt.Broken)
	}

	status, rolledBack, verified = readHintStatus(t, ctx, tuner, agedID)
	if status == "retired" || status == "broken" {
		t.Errorf("aged hint status = %q, want it kept installed", status)
	}
	if rolledBack || verified {
		t.Errorf("aged hint rolled_back=%v verified=%v, want neither", rolledBack, verified)
	}
	if !hintReverifyDue(t, ctx, pool, agedID) {
		t.Error("aged hint should be stamped reverify_due")
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
