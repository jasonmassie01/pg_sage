package tuning

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// tuning-06 review: the analyzer resolves by absence only the categories a
// producer evaluated or emitted. The agent judges every open query_tuning
// finding, so it reports the category evaluated; otherwise the last open
// query_tuning finding keepOpen let go would never resolve.
func TestTune_ReportsQueryTuningEvaluated(t *testing.T) {
	h := newHarness(t)
	h.store.open = []analyzer.Finding{queryHintFinding(qhID)}
	h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true,
		HintStatus: "active"}}
	prev, cur := shareSnaps(10, 100, 5) // no case: nothing is asked
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if _, ok := byIdent(out.Findings, "queryid:4242001"); ok {
		t.Fatalf("an active hint's finding was re-emitted: %+v", out.Findings)
	}
	if !slices.Contains(out.Evaluated, "query_tuning") {
		t.Fatalf("query_tuning is not evaluated, so the finding cannot resolve: %v",
			out.Evaluated)
	}
	if !slices.Equal(out.Evaluated[:len(out.Evaluated)-1], analyzer.TuningCategories()) {
		t.Fatalf("evaluated = %v, want the tuning categories plus query_tuning",
			out.Evaluated)
	}
}

// Error propagation: when the open findings are unreadable nothing is
// evaluated, query_tuning included.
func TestTune_UnreadableOpenFindingsEvaluateNoQueryTuning(t *testing.T) {
	h := newHarness(t)
	h.store.openErr = context.DeadlineExceeded
	prev, cur := shareSnaps(10, 100, 5)
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err == nil {
		t.Fatal("an unreadable open-findings read returned no error")
	}
	if slices.Contains(out.Evaluated, "query_tuning") || len(out.Evaluated) != 0 {
		t.Fatalf("evaluated %v on an unreadable read", out.Evaluated)
	}
}

// A finding without installable SQL (the tuner's operator-hint or advisory
// finding) recorded no query_hints row of its own: an older row's status
// must not resolve it, or the tuner re-opens it after every cooldown.
func TestQueryHint_UnownedFindingIgnoresOldHintStatus(t *testing.T) {
	for _, status := range []string{"rolled_back", "retired", "expired", "rejected"} {
		h := newHarness(t)
		h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true,
			HintStatus: status}}
		open := queryHintFinding(qhID)
		open.RecommendedSQL = ""
		open.Detail = map[string]any{"queryid": qhID, "operator_hint_present": true}
		outs := keepCycles(h, []analyzer.Finding{open}, 1, nil, nil)
		got, ok := byIdent(outs[0], "queryid:4242001")
		if !ok || !reflect.DeepEqual(got, open) {
			t.Fatalf("status %s resolved or changed an unowned finding: %+v", status, outs[0])
		}
	}
	// It still resolves on its statement's evidence.
	h := newHarness(t)
	h.store.qstates = map[int64]QueryState{qhID: {Present: false, HintStatus: "retired"}}
	open := queryHintFinding(qhID)
	open.RecommendedSQL = ""
	outs := keepCycles(h, []analyzer.Finding{open}, 3, nil, nil)
	if _, ok := byIdent(outs[2], "queryid:4242001"); ok {
		t.Fatal("an unowned finding absent for 3 cycles is still open")
	}
	if _, ok := byIdent(outs[1], "queryid:4242001"); !ok {
		t.Fatal("an unowned finding resolved before N absent cycles")
	}
}
