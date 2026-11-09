package tuner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The dormant state on the database (tuning-01): revalidation reads it,
// reconciliation rolls it back once the installed hint is gone, removal
// leaves it installed, the cooldown bootstrap counts it as covered, and
// recordRevalidation writes each decision's state.

// No concurrent access tests here: TestRevalidate_ConcurrentWithTune covers
// the passes that share these writes.

func seedRevalHintStatus(t *testing.T, ctx context.Context, tu *Tuner,
	qid int64, status string) int64 {
	t.Helper()
	id := seedRevalHint(t, ctx, tu, qid, 1, nil)
	if _, err := tu.pool.Exec(ctx,
		"UPDATE sage.query_hints SET status = $2 WHERE id = $1", id, status); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
	return id
}

func TestLoadHintsForRevalidation_IncludesDormant(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	cleanRevalHints(t, ctx, tu)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tu) })
	dormant := seedRevalHintStatus(t, ctx, tu, revalTestQIDBase+60, "dormant")
	broken := seedRevalHintStatus(t, ctx, tu, revalTestQIDBase+61, "broken")
	rolledBack := seedRevalHintStatus(t, ctx, tu, revalTestQIDBase+62, "rolled_back")

	hints, err := tu.loadHintsForRevalidation(ctx)
	if err != nil {
		t.Fatalf("loadHintsForRevalidation: %v", err)
	}
	seen := map[int64]string{}
	for _, h := range hints {
		seen[h.ID] = h.Status
	}
	if seen[dormant] != "dormant" {
		t.Errorf("dormant hint missing from the revalidation set (got %v)", seen[dormant])
	}
	for _, id := range []int64{broken, rolledBack} {
		if _, ok := seen[id]; ok {
			t.Errorf("hint %d (%s) must not be revalidated", id, seen[id])
		}
	}
}

func TestRecordRevalidation_Transitions(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	cleanRevalHints(t, ctx, tu)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tu) })
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	id := seedRevalHint(t, ctx, tu, revalTestQIDBase+70, 1, ptrInt64(50))
	h := hintRow{ID: id, Status: "active"}
	d := hintDecision{action: hintActionDormant, resetAt: &reset, reverifyDue: true}
	if err := tu.recordRevalidation(ctx, h, d); err != nil {
		t.Fatalf("record dormant: %v", err)
	}
	s := readHintState(t, ctx, pool, id)
	if s.status != "dormant" || s.checkpoint == nil || *s.checkpoint != 50 ||
		s.resetAt == nil || !s.resetAt.Equal(reset) || s.reverify == nil {
		t.Fatalf("after dormant = %+v, want dormant, checkpoint 50 kept, reset, due", s)
	}
	firstDue := *s.reverify

	h.Status = "dormant"
	d = hintDecision{action: hintActionReset, checkpoint: ptrInt64(2), reverifyDue: true}
	if err := tu.recordRevalidation(ctx, h, d); err != nil {
		t.Fatalf("record reset: %v", err)
	}
	s = readHintState(t, ctx, pool, id)
	if s.status != "dormant" || *s.checkpoint != 2 || !s.resetAt.Equal(reset) ||
		!s.reverify.Equal(firstDue) {
		t.Fatalf("after reset = %+v, want dormant, checkpoint 2, reset and due kept", s)
	}

	d = hintDecision{action: hintActionBroken}
	if err := tu.recordRevalidation(ctx, h, d); err != nil {
		t.Fatalf("record broken: %v", err)
	}
	status, rolledBack, verified := readHintStatus(t, ctx, tu, id)
	if status != "broken" || !rolledBack || verified {
		t.Fatalf("after broken: status=%q rolled_back=%v verified=%v", status,
			rolledBack, verified)
	}
}

// A hint the reconcile step rolled back while the pass ran is not
// overwritten: only active and dormant rows are written.
func TestRecordRevalidation_SkipsRowsThatLeftTheLiveStates(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	cleanRevalHints(t, ctx, tu)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tu) })
	id := seedRevalHintStatus(t, ctx, tu, revalTestQIDBase+71, "rolled_back")
	d := hintDecision{action: hintActionKeep, checkpoint: ptrInt64(9)}
	if err := tu.recordRevalidation(ctx, hintRow{ID: id, Status: "active"}, d); err != nil {
		t.Fatalf("record: %v", err)
	}
	if s := readHintState(t, ctx, pool, id); s.status != "rolled_back" || s.checkpoint != nil {
		t.Fatalf("rolled-back hint = %+v, want untouched", s)
	}
}

// An error decision writes nothing.
func TestRecordRevalidation_ErrorWritesNothing(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	cleanRevalHints(t, ctx, tu)
	t.Cleanup(func() { cleanRevalHints(t, ctx, tu) })
	id := seedRevalHint(t, ctx, tu, revalTestQIDBase+72, 1, nil)
	d := hintDecision{action: hintActionError, checkpoint: ptrInt64(9)}
	if err := tu.recordRevalidation(ctx, hintRow{ID: id, Status: "active"}, d); err != nil {
		t.Fatalf("record: %v", err)
	}
	if s := readHintState(t, ctx, pool, id); s.status != "active" || s.checkpoint != nil {
		t.Fatalf("hint after an error decision = %+v, want untouched", s)
	}
}

// Reconcile moves a dormant hint to rolled_back once its installed row is
// gone, like an active one.
func TestReconcileHintStatuses_DormantRolledBack(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES ($1, 'x', 'disk_sort', 'dormant')`,
		hintStatusQueryID); err != nil {
		t.Fatalf("seed dormant: %v", err)
	}
	tu.reconcileHintStatuses(ctx)
	if got := hintStatuses(t, pool); len(got) != 1 || got[0] != "rolled_back" {
		t.Fatalf("uninstalled dormant hint = %v, want [rolled_back]", got)
	}
}

// A dormant hint is still installed and wanted: an older retired row for the
// same query must not trigger its removal, and it counts as covered.
func TestHintRemoval_DormantHintKept(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	for _, status := range []string{"retired", "dormant"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
			(queryid, hint_text, symptom, status) VALUES ($1, 'SeqScan(t)', 'x', $2)`,
			hintStatusQueryID, status); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}
	if _, err := pool.Exec(ctx, BuildInsertSQL(hintStatusQueryID, "SeqScan(t)")); err != nil {
		t.Fatalf("install hint: %v", err)
	}
	for _, f := range tu.hintRemovalFindings(ctx) {
		if f.ObjectIdentifier == "queryid:987650001" {
			t.Fatalf("removal finding for a dormant hint: %s", f.Title)
		}
	}
	// tuning-02: covered means refused by the durable check (the map
	// bootstrap it replaced is gone).
	if err := readyTuner(pool).coolingErr(ctx, hintStatusQueryID); !errors.Is(err, ErrHintExists) {
		t.Fatalf("dormant hint not counted as covered: %v", err)
	}
}
