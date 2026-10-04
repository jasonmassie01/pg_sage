package tuning

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// The per-cycle cap is applied before anything is recorded: a hint the
// cap cuts leaves no trace in sage.query_hints and no cooldown, only the
// counted "capped" metric and the cap log line.

// threeStatementPair is an interval with three dominant statements on
// public.orders, weighted 201 > 202 > 203, so each is its own top case.
func threeStatementPair() (prev, cur *collector.Snapshot) {
	tbl := []collector.TableStats{table("public", "orders", 1_000_000, 1000)}
	texts := map[int64]string{
		201: "SELECT * FROM public.orders WHERE customer_id = $1",
		202: "SELECT * FROM public.orders WHERE status = $1",
		203: "SELECT * FROM public.orders WHERE created_at > $1",
	}
	ms := map[int64]float64{201: 9000, 202: 8000, 203: 7000}
	var p, c []collector.QueryStats
	for _, id := range []int64{201, 202, 203} {
		p = append(p, stmt(id, texts[id], 1000, ms[id]))
		c = append(c, stmt(id, texts[id], 1600, ms[id]*1.6))
	}
	return snapAt(t0, p, tbl, nil), snapAt(t0.Add(5*time.Minute), c, tbl, nil)
}

func hintAnswer(t *testing.T, qid int64) func([]llm.Message) (llm.ToolResult, error) {
	return answer(proposalsJSON(t, map[string]any{"type": "query_hint", "queryid": qid,
		"hint": "SeqScan(orders)", "rationale": "the index scan is slower here",
		"evidence": []string{"S1"}, "expected_change_pct": -30}))
}

func tuneThree(t *testing.T, h *harness) []int64 {
	t.Helper()
	prev, cur := threeStatementPair()
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	var ids []int64
	for _, f := range out.Findings {
		if f.Category == "query_tuning" {
			ids = append(ids, f.Detail["queryid"].(int64))
		}
	}
	return ids
}

func TestTune_HintCapRecordsExactlyTheKept(t *testing.T) {
	const capN = 2
	s := defaultSettings()
	s.Tuning.MaxProposalsPerCycle = capN
	h := newHarnessWith(t, s, hintAnswer(t, 201), hintAnswer(t, 202), hintAnswer(t, 203))
	emitted := tuneThree(t, h)
	if len(h.hints.checked) != capN+1 {
		t.Fatalf("checked %d hints, want all %d admitted", len(h.hints.checked), capN+1)
	}
	if len(h.hints.recorded) != capN {
		t.Fatalf("recorded %d hints, want exactly the cap %d: %+v", len(h.hints.recorded),
			capN, h.hints.recorded)
	}
	if h.hints.recorded[0].QueryID != 201 || h.hints.recorded[1].QueryID != 202 {
		t.Fatalf("recorded %+v: the two best-ranked cases", h.hints.recorded)
	}
	if len(emitted) != capN || emitted[0] != 201 || emitted[1] != 202 {
		t.Fatalf("emitted %v: the recorded hints, nothing else", emitted)
	}
	if got := h.agent.Stats().ProposalsCapped; got != 1 {
		t.Fatalf("capped counter = %d, want 1", got)
	}
	if !h.logs.contains("cap") {
		t.Fatal("cutting proposals at the cap is logged")
	}
}

func TestTune_HintCapBoundaryRecordsAllAtTheCap(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxProposalsPerCycle = 3
	h := newHarnessWith(t, s, hintAnswer(t, 201), hintAnswer(t, 202), hintAnswer(t, 203))
	if emitted := tuneThree(t, h); len(emitted) != 3 || len(h.hints.recorded) != 3 {
		t.Fatalf("at the cap all are kept: emitted %v, recorded %d", emitted,
			len(h.hints.recorded))
	}
	if got := h.agent.Stats().ProposalsCapped; got != 0 {
		t.Fatalf("nothing capped, counter = %d", got)
	}
}

func TestTune_HintTheTunerWillNotRecordIsDropped(t *testing.T) {
	h := newHarness(t, hintAnswer(t, 201), hintAnswer(t, 202), hintAnswer(t, 203))
	h.hints.recordErr = tuner.ErrHintExists
	if emitted := tuneThree(t, h); len(emitted) != 0 {
		t.Fatalf("emitted %v: a hint that was not recorded is not proposed", emitted)
	}
	if !h.logs.contains("not recorded") {
		t.Fatal("the dropped hint is logged")
	}
}
