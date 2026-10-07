package tuning

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// lifeos (v1.10.0): pg_sage built (status, fact_type, quality_score) WHERE
// valid_to IS NULL AND deleted_at IS NULL AND quality_score IS NOT NULL,
// then ten minutes later the same keys with the weaker predicate, which
// made the first redundant; HypoPG verified each on its own. An index
// candidate is now judged against the existing and in-flight indexes of
// its table: refused when one already serves it, refused when it would
// make an in-flight one redundant (an existing one is replaced: 2.3), one
// proposal per shape family per cycle, and measured with the in-flight
// ones present as hypothetical indexes.

const inflightLive = "CREATE INDEX CONCURRENTLY orders_live_status_created ON " +
	"public.orders (status, created_at) WHERE deleted_at IS NULL AND created_at IS NOT NULL"

func indexProposal(ddl string) Proposal {
	p := createProposal()
	p.DDL = ddl
	return p
}

func TestJudge_CandidateCoveredByAnInFlightIndexIsRefused(t *testing.T) {
	h := newHarness(t)
	h.store.queue = []string{"CREATE INDEX CONCURRENTLY orders_c_all ON public.orders " +
		"(customer_id, created_at)"}
	j := judgeOne(t, h, nil, createProposal())
	if j.Verdict != VerdictRejected || j.Reason != ReasonDuplicate {
		t.Fatalf("an in-flight index already serves it: %+v", j)
	}
	if len(h.indexes.admitted) != 0 {
		t.Fatal("refused before any what-if")
	}
}

func TestJudge_CandidateThatWouldMakeAnIndexRedundantIsRefused(t *testing.T) {
	h := newHarness(t)
	h.store.queue = []string{inflightLive}
	weaker := "CREATE INDEX CONCURRENTLY orders_status_created_current ON public.orders " +
		"(status, created_at) WHERE deleted_at IS NULL"
	j := judgeOne(t, h, nil, indexProposal(weaker))
	if j.Verdict != VerdictRejected || j.Reason != ReasonSubsumes ||
		!strings.Contains(j.Detail, "orders_live_status_created") {
		t.Fatalf("it would make the in-flight index redundant: %+v", j)
	}
	// Making an existing index redundant is a replacement since 2.3
	// (replace_test.go).
}

func TestJudge_OneProposalPerShapeFamilyPerCycle(t *testing.T) {
	h := newHarness(t)
	prev, _ := ordersPair()
	cur := validationSnap()
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0), nil, h.store.rejected)
	v.prepare(context.Background(), prev, nil)
	first := v.judge(context.Background(), ordersCase(), ordersEvidence(),
		indexProposal("CREATE INDEX CONCURRENTLY orders_c_created ON public.orders "+
			"(customer_id, created_at)"))
	second := v.judge(context.Background(), ordersCase(), ordersEvidence(),
		indexProposal("CREATE INDEX CONCURRENTLY orders_c_id ON public.orders "+
			"(customer_id, id)"))
	if first.Verdict != VerdictAdmitted || second.Verdict != VerdictRejected ||
		second.Reason != ReasonDuplicate {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

func TestJudge_WhatIfIncludesInFlightIndexes(t *testing.T) {
	h := newHarness(t)
	h.store.queue = []string{inflightLive}
	var alongside []string
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		alongside = rec.Alongside
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitRejected,
			Reason: "no gain with the in-flight index present"}
	}
	j := judgeOne(t, h, nil, createProposal())
	if !slices.Contains(alongside, inflightLive) || j.Reason != ReasonWhatIfRejected {
		t.Fatalf("alongside %v judged %+v", alongside, j)
	}
}

func TestJudge_InFlightUnreadableRefusesIndexCreates(t *testing.T) {
	h := newHarness(t)
	h.store.queueErr = errFake
	if j := judgeOne(t, h, nil, createProposal()); j.Verdict != VerdictRejected ||
		j.Reason != ReasonUnavailable {
		t.Fatalf("without the in-flight indexes a duplicate cannot be ruled out: %+v", j)
	}
}

func TestTune_OpenIndexFindingIsInFlightForOtherCases(t *testing.T) {
	h := newHarness(t, answer(proposalsJSON(t, map[string]any{"type": "index_create",
		"ddl": "CREATE INDEX CONCURRENTLY orders_status_current ON public.orders " +
			"(status, created_at) WHERE deleted_at IS NULL",
		"evidence": []string{"S1"}, "expected_change_pct": -50})))
	open := agentFinding("top_statement:201", optimizer.OptimizerCategory,
		"public.orders|btree(status,created_at)")
	open.RecommendedSQL = inflightLive
	h.store.open = []analyzer.Finding{open}
	prev, cur := threeStatementPair()
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if h.model.callCount() == 0 {
		t.Fatal("another case of the table is asked")
	}
	for _, f := range out.Findings {
		if f.RecommendedSQL != open.RecommendedSQL &&
			f.Category == optimizer.OptimizerCategory {
			t.Fatalf("a create that would make the open proposal redundant: %+v", f)
		}
	}
}

func TestPacket_ListsInFlightIndexes(t *testing.T) {
	h := newHarness(t)
	prev, cur := ordersPair()
	w := ClassifyWorkload(cur, nil, t0)
	cs := DetectCases(cur, prev, w, DefaultThresholds())
	pk := h.agent.packetFor(context.Background(), cs[0], cur, w, nil,
		map[string][]string{"public.orders": {inflightLive}})
	if !strings.Contains(pk.Text, "in flight on public.orders") ||
		!strings.Contains(pk.Text, "orders_live_status_created") {
		t.Fatalf("the model sees the in-flight indexes: %s", pk.Text)
	}
}
