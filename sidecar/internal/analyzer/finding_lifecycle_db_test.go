package analyzer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rules-06 on real PostgreSQL: per-row upsert errors do not stop the
// batch, publishing filters every finding by active suppression, and a
// condition that persists after an action reopens the action's row.

const lifecycleActionMarker = "test_phase2_rules06_action"

// insertActionResolved stores a finding resolved by an action at
// resolvedAgo before now and returns the finding and action_log ids.
func insertActionResolved(
	t *testing.T, pool *pgxpool.Pool, f Finding, resolvedAgo time.Duration,
) (findingID, actionID int64) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
			f.Category)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.action_log WHERE sql_executed = $1`,
			lifecycleActionMarker)
	})
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.action_log (action_type, finding_id, sql_executed, outcome)
		 VALUES ('vacuum', 0, $1, 'success') RETURNING id`,
		lifecycleActionMarker).Scan(&actionID)
	if err != nil {
		t.Fatalf("insert action_log: %v", err)
	}
	err = pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		    (category, severity, object_type, object_identifier, title, detail,
		     status, resolved_at, acted_on_at, action_log_id)
		 VALUES ($1, $2, 'table', $3, 'acted on', '{"before":true}'::jsonb,
		     'resolved', now() - make_interval(secs => $4),
		     now() - make_interval(secs => $4), $5)
		 RETURNING id`,
		f.Category, f.Severity, f.ObjectIdentifier, resolvedAgo.Seconds(),
		actionID).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert action-resolved finding: %v", err)
	}
	return findingID, actionID
}

// dbNow is the server clock, so evidence times line up with resolved_at.
func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("select now(): %v", err)
	}
	return now
}

func insertSuppressed(t *testing.T, pool *pgxpool.Pool, f Finding) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sage.findings
		    (category, severity, object_type, object_identifier, title, detail, status)
		 VALUES ($1, 'warning', 'table', $2, 'suppressed', '{}'::jsonb, 'suppressed')`,
		f.Category, f.ObjectIdentifier)
	if err != nil {
		t.Fatalf("insert suppressed finding: %v", err)
	}
}

func countCategoryRows(t *testing.T, pool *pgxpool.Pool, cat string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.findings WHERE category = $1`, cat).Scan(&n)
	if err != nil {
		t.Fatalf("count category rows: %v", err)
	}
	return n
}

func latestHas(a *Analyzer, f Finding) bool {
	for _, lf := range a.LatestFindings() {
		if lf.Category == f.Category && lf.ObjectIdentifier == f.ObjectIdentifier {
			return true
		}
	}
	return false
}

// fiveFindings returns five findings of one category; the third has a
// NUL byte in its title, which PostgreSQL rejects (SQLSTATE 22021).
func fiveFindings(cat string) []Finding {
	out := make([]Finding, 5)
	for i := range out {
		out[i] = lifecycleFinding(cat, fmt.Sprintf("public.t%d", i+1), "warning")
	}
	out[2].Title = "bad\x00title"
	return out
}

func TestLifecycle_UpsertContinuesPastRowError(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ff := fiveFindings("test_phase2_rules06_err")
	insertSuppressed(t, pool, ff[4])

	res, err := UpsertFindingsWithResult(context.Background(), pool, ff)
	if err == nil {
		t.Fatal("err = nil, want the third finding's insert error")
	}
	if !strings.Contains(err.Error(), ff[2].ObjectIdentifier) ||
		!strings.Contains(err.Error(), ff[2].Category) {
		t.Fatalf("err = %q, want it to name %s/%s", err, ff[2].Category,
			ff[2].ObjectIdentifier)
	}
	if strings.Contains(err.Error(), ff[3].ObjectIdentifier) {
		t.Fatalf("err = %q names a finding that persisted", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].ObjectIdentifier != ff[2].ObjectIdentifier {
		t.Fatalf("Failed = %+v, want only %s", res.Failed, ff[2].ObjectIdentifier)
	}
	if len(res.Opened) != 3 {
		t.Fatalf("Opened = %d, want 3 (t1, t2, t4)", len(res.Opened))
	}
	if !res.IsSuppressed(ff[4]) {
		t.Fatalf("Suppressed = %+v, want %s", res.Suppressed, ff[4].ObjectIdentifier)
	}
	for _, i := range []int{0, 1, 3} {
		if n := countFindingRows(t, pool, ff[i].Category, ff[i].ObjectIdentifier,
			"open"); n != 1 {
			t.Fatalf("open rows for %s = %d, want 1", ff[i].ObjectIdentifier, n)
		}
	}
}

func TestLifecycle_InvalidDetailDoesNotStopBatch(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	bad := lifecycleFinding("test_phase2_rules06_json", "public.bad", "info")
	bad.Detail = map[string]any{"fn": func() {}}
	good := lifecycleFinding("test_phase2_rules06_json", "public.good", "info")

	res, err := UpsertFindingsWithResult(context.Background(), pool, []Finding{bad, good})
	if err == nil || !strings.Contains(err.Error(), "public.bad") {
		t.Fatalf("err = %v, want the marshal error naming public.bad", err)
	}
	if len(res.Failed) != 1 || len(res.Opened) != 1 ||
		res.Opened[0].ObjectIdentifier != "public.good" {
		t.Fatalf("Failed=%+v Opened=%+v, want bad failed and good opened",
			res.Failed, res.Opened)
	}
	if n := countFindingRows(t, pool, good.Category, "public.good", "open"); n != 1 {
		t.Fatalf("open rows for public.good = %d, want 1", n)
	}
}

// CHECK-17: four rows exist (t1, t2, t4 open; t5 suppressed) and the
// suppressed identity is not published although it came after the error.
func TestLifecycle_CycleWithRowErrorPublishesUnsuppressed(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ff := fiveFindings("test_phase2_rules06_cycle")
	insertSuppressed(t, pool, ff[4])

	a.finalizeCycle(context.Background(), ff, map[string]bool{ff[0].Category: true})

	if n := countCategoryRows(t, pool, ff[0].Category); n != 4 {
		t.Fatalf("rows in category = %d, want 4", n)
	}
	if n := countFindingRows(t, pool, ff[4].Category, ff[4].ObjectIdentifier,
		"open"); n != 0 {
		t.Fatalf("open rows for suppressed t5 = %d, want 0", n)
	}
	if latestHas(a, ff[4]) {
		t.Fatal("suppressed t5 published in LatestFindings")
	}
	for _, i := range []int{0, 1, 2, 3} {
		if !latestHas(a, ff[i]) {
			t.Fatalf("unsuppressed %s missing from LatestFindings", ff[i].ObjectIdentifier)
		}
	}
}

func TestLifecycle_SuppressionQueryFailureKeepsPreviousSet(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	keep := lifecycleFinding("test_phase2_rules06_aj", "public.keep", "warning")
	gone := lifecycleFinding("test_phase2_rules06_aj", "public.gone", "warning")
	eval := map[string]bool{keep.Category: true}
	a.finalizeCycle(ctx, []Finding{keep, gone}, eval)

	a.suppressions = &countingQuerier{err: errors.New("canceling statement due to timeout")}
	a.finalizeCycle(ctx, []Finding{keep}, eval)

	if !latestHas(a, keep) || !latestHas(a, gone) || len(a.LatestFindings()) != 2 {
		t.Fatalf("LatestFindings = %+v, want the previous cycle's two findings",
			a.LatestFindings())
	}
	if n := countFindingRows(t, pool, gone.Category, gone.ObjectIdentifier,
		"open"); n != 1 {
		t.Fatalf("open rows for public.gone = %d, want 1 (nothing resolves)", n)
	}
	if n := countFindingRows(t, pool, gone.Category, gone.ObjectIdentifier,
		"resolved"); n != 0 {
		t.Fatalf("resolved rows for public.gone = %d, want 0", n)
	}
}

// lifecycleAnalyzerAt is lifecycleAnalyzer with the default intervals
// (analyzer 600 s, collector 60 s: a 1200 s reopen window) and a cycle
// whose evidence time the test sets in a.eval.collectedAt.
func lifecycleAnalyzerAt(t *testing.T) (*Analyzer, *pgxpool.Pool, *mockDispatcher) {
	t.Helper()
	a, pool, d := lifecycleAnalyzer(t)
	a.cfg.Analyzer.IntervalSeconds = 600
	a.cfg.Collector.IntervalSeconds = 60
	a.eval = newCycleEval()
	return a, pool, d
}

// CHECK-18: one row per identity, reopened with persisted_after_action.
func TestLifecycle_PersistedAfterActionReopensSameRow(t *testing.T) {
	a, pool, d := lifecycleAnalyzerAt(t)
	f := lifecycleFinding("test_phase2_rules06_persist", "public.orders", "critical")
	id, actionID := insertActionResolved(t, pool, f, 600*time.Second)
	a.eval.collectedAt = dbNow(t, pool)
	eval := map[string]bool{f.Category: true}

	a.finalizeCycle(context.Background(), []Finding{f}, eval)
	a.eval.collectedAt = dbNow(t, pool)
	a.finalizeCycle(context.Background(), []Finding{f}, eval)

	if n := countCategoryRows(t, pool, f.Category); n != 1 {
		t.Fatalf("rows for identity = %d, want exactly 1", n)
	}
	var status string
	var linked *int64
	var resolvedAt *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT status, (detail->>'persisted_after_action')::bigint, resolved_at
		   FROM sage.findings WHERE id = $1`, id).Scan(&status, &linked, &resolvedAt)
	if err != nil {
		t.Fatalf("select reopened row: %v", err)
	}
	if status != "open" || linked == nil || *linked != actionID || resolvedAt != nil {
		t.Fatalf("row = status %q, persisted_after_action %v, resolved_at %v; "+
			"want open, %d, NULL", status, linked, resolvedAt, actionID)
	}
	if d.count() != 1 {
		t.Fatalf("critical dispatches = %d, want exactly 1 across two cycles", d.count())
	}
}

func TestLifecycle_StaleSnapshotReopensNothing(t *testing.T) {
	a, pool, d := lifecycleAnalyzerAt(t)
	f := lifecycleFinding("test_phase2_rules06_stale", "public.orders", "critical")
	id, _ := insertActionResolved(t, pool, f, 60*time.Second)
	a.eval.collectedAt = dbNow(t, pool).Add(-61 * time.Second)

	a.finalizeCycle(context.Background(), []Finding{f}, map[string]bool{f.Category: true})

	if n := countCategoryRows(t, pool, f.Category); n != 1 {
		t.Fatalf("rows = %d, want 1 (the resolved row only)", n)
	}
	if n := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved"); n != 1 {
		t.Fatalf("resolved rows = %d, want 1 (row %d untouched)", n, id)
	}
	if d.count() != 0 {
		t.Fatalf("dispatches for stale evidence = %d, want 0", d.count())
	}
}

func TestLifecycle_PastWindowOpensNewRow(t *testing.T) {
	a, pool, _ := lifecycleAnalyzerAt(t)
	f := lifecycleFinding("test_phase2_rules06_late", "public.orders", "warning")
	id, _ := insertActionResolved(t, pool, f, 1201*time.Second)
	a.eval.collectedAt = dbNow(t, pool)

	a.finalizeCycle(context.Background(), []Finding{f}, map[string]bool{f.Category: true})

	if n := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); n != 1 {
		t.Fatalf("open rows = %d, want 1 new row", n)
	}
	var status string
	var hasKey bool
	err := pool.QueryRow(context.Background(),
		`SELECT status, detail ? 'persisted_after_action' FROM sage.findings
		  WHERE id = $1`, id).Scan(&status, &hasKey)
	if err != nil || status != "resolved" || hasKey {
		t.Fatalf("old row = %q key=%v err=%v, want resolved and untouched",
			status, hasKey, err)
	}
}

// Concurrent access: two upserters racing on one new identity end with
// one open row (idx_findings_dedup) and neither reports an error.
func TestLifecycle_ConcurrentUpsertOneOpenRow(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	for i := 0; i < 15; i++ {
		f := lifecycleFinding("test_phase2_rules06_race", fmt.Sprintf("public.r%d", i),
			"warning")
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for g := range errs {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				_, errs[g] = UpsertFindingsWithResult(context.Background(), pool,
					[]Finding{f})
			}(g)
		}
		close(start)
		wg.Wait()
		for g, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d upserter %d: %v", i, g, err)
			}
		}
		if n := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); n != 1 {
			t.Fatalf("iteration %d: open rows = %d, want 1", i, n)
		}
	}
}

// ResolveClearedFindings keeps resolving after one UPDATE fails. The
// failure is a trigger that rejects resolving public.boom.
func TestLifecycle_ResolveClearedContinuesPastRowError(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ctx := context.Background()
	cat := "test_phase2_rules06_resolve"
	installResolveBomb(t, pool)
	var ff []Finding
	for _, ident := range []string{"public.a", "public.boom", "public.c"} {
		ff = append(ff, lifecycleFinding(cat, ident, "warning"))
	}
	if err := UpsertFindings(ctx, pool, ff); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cleared, err := ResolveClearedFindings(ctx, pool, nil, cat)
	if err == nil || !strings.Contains(err.Error(), "rules06 boom") {
		t.Fatalf("err = %v, want the trigger's error", err)
	}
	got := map[string]bool{}
	for _, c := range cleared {
		got[c.ObjectIdentifier] = true
	}
	if len(cleared) != 2 || !got["public.a"] || !got["public.c"] {
		t.Fatalf("cleared = %+v, want public.a and public.c", cleared)
	}
	if n := countFindingRows(t, pool, cat, "public.boom", "open"); n != 1 {
		t.Fatalf("open rows for public.boom = %d, want 1", n)
	}
}

func installResolveBomb(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION public.test_phase2_rules06_bomb() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.object_identifier = 'public.boom' AND NEW.status = 'resolved' THEN
    RAISE EXCEPTION 'rules06 boom';
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS test_phase2_rules06_bomb ON sage.findings;
CREATE TRIGGER test_phase2_rules06_bomb BEFORE UPDATE ON sage.findings
  FOR EACH ROW EXECUTE FUNCTION public.test_phase2_rules06_bomb();`)
	if err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_phase2_rules06_bomb
			ON sage.findings;
			DROP FUNCTION IF EXISTS public.test_phase2_rules06_bomb()`)
	})
}
