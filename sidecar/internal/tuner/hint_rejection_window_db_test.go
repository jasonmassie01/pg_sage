package tuner

import (
	"testing"
)

// Boundary (tuning-06 review): the 30-day window of an operator rejection
// runs from the decision, not from the proposal. A proposal answered on
// its 29th day must not be proposed again the next day.
func TestHintStatus_RejectionWindowRunsFromTheDecision(t *testing.T) {
	const qid = int64(9_260_071)
	pool, ctx := hintScopeDB(t, qid)
	lifecycleDB(t, qid)
	tu := ownershipTuner(pool)
	if fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err != nil ||
		len(findingFor(fs, qid)) != 1 {
		t.Fatalf("first proposal: %+v, %v", fs, err)
	}
	ageHint(t, pool, qid, "29 days 12 hours")
	seedDecision(t, pool, qid, "query_tuning", "rejected", 1, "1 day")
	if rejected, _ := tu.reconcileLifecycle(ctx); rejected != 1 {
		t.Fatalf("%d proposals rejected, want 1", rejected)
	}
	ageHint(t, pool, qid, "31 days")
	tu.recentlyTuned = map[int64]int{}
	if fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err != nil ||
		len(findingFor(fs, qid)) != 0 {
		t.Fatalf("a hint rejected a day ago was proposed again: %+v, %v", fs, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET decided_at =
		now() - interval '30 days 1 minute' WHERE finding_id IN (SELECT id FROM
		sage.findings WHERE object_identifier = $1)`, "queryid:9260071"); err != nil {
		t.Fatal(err)
	}
	tu.recentlyTuned = map[int64]int{}
	if fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err != nil ||
		len(findingFor(fs, qid)) != 1 {
		t.Fatalf("a rejection decided 30 days ago still binds: %+v, %v", fs, err)
	}
}
