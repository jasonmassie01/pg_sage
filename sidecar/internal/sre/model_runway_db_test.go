package sre

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Model-turn compatibility for the runway families (Sage SRE M6): the
// model sees the runway family and the graph version, ranks the family's
// open hypotheses and cites runway evidence; while the graph is
// inconclusive it may ask for the runway trend probe with a typed window.
// It can neither rank another family's node nor move the deterministic
// root, and its ranking never changes the stored hypotheses.

var openLine = regexp.MustCompile(`- ([a-z_]+) \[`)

// promptOpenNodes lists the hypotheses the prompt asks the model to rank, in
// prompt order.
func promptOpenNodes(t *testing.T, body string) []string {
	t.Helper()
	start := strings.Index(body, "Open hypotheses (rank all of these):")
	end := strings.Index(body, "Ruled out (do not rank):")
	if start < 0 || end < start {
		t.Fatalf("prompt has no open hypotheses section: %s", body)
	}
	var out []string
	for _, m := range openLine.FindAllStringSubmatch(body[start:end], -1) {
		out = append(out, m[1])
	}
	return out
}

func modelSeqTrigger(key string) Trigger {
	tr := seqTrigger()
	tr.CaseID += ":" + key
	tr.IdempotencyKey = "runway:" + key
	return tr
}

// seqReview ranks the prompt's open hypotheses in order and narrates one
// claim citing the first sequence runway sample.
func seqReview(t *testing.T) fakeReply {
	return toolReply(func(body string) string {
		return wireReview{Ranking: promptOpenNodes(t, body), Claims: []wireClaim{{
			Text: "The sequence is owned by a column narrower than the sequence.",
			EvidenceIDs: []string{aliasOf(t, body, probes.SequenceRunwayProbe,
				"ok")}}}}.json()
	})
}

func TestModelTurn_RunwayReviewStoredBesideTheGraph(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	plain, _ := testCoordinator(t, ctx, st, narrowColumnRunner(), nil)
	base := startAndRun(t, ctx, plain, modelSeqTrigger("m6-model-base"))
	m := newFakeModel(t, seqReview(t))
	c, _ := modelCoordinator(t, ctx, st, narrowColumnRunner(), m.client())
	inv := startAndRun(t, ctx, c, modelSeqTrigger("m6-model-agree"))
	if inv.State != StateConcluded || inv.Summary.Root != "column_narrower_than_sequence" {
		t.Fatalf("investigation = %s root %q (%s)", inv.State, inv.Summary.Root,
			inv.Summary.Reason)
	}
	body := m.body(t, 0)
	if !strings.Contains(body, "sequence_runway incident (trigger sequence_runway), "+
		"causal graph causal-v3") {
		t.Fatalf("prompt does not name the runway family and graph: %s", body)
	}
	r := inv.Summary.ModelRanking
	if r == nil || len(r.Nodes) == 0 || r.Nodes[0] != "column_narrower_than_sequence" ||
		strings.Join(r.Nodes, ",") != strings.Join(promptOpenNodes(t, body), ",") {
		t.Fatalf("model ranking = %+v", r)
	}
	n := inv.Summary.Narrative
	if n == nil || len(n.Claims) != 1 || len(n.Claims[0].EvidenceIDs) != 1 {
		t.Fatalf("narrative = %+v", n)
	}
	assertEvidenceProbe(t, st, inv, n.Claims[0].EvidenceIDs[0], probes.SequenceRunwayProbe)
	want, _ := st.Hypotheses(ctx, base.Scope, base.ID)
	got, _ := st.Hypotheses(ctx, inv.Scope, inv.ID)
	if len(got) == 0 || len(got) != len(want) {
		t.Fatalf("hypotheses %d vs deterministic %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Node != want[i].Node || got[i].Status != want[i].Status ||
			got[i].Confidence != want[i].Confidence {
			t.Fatalf("hypothesis %d = %+v, deterministic %+v", i, got[i], want[i])
		}
	}
}

func assertEvidenceProbe(t *testing.T, st *PostgresStore, inv Investigation, id UUID,
	probe probes.ID) {
	t.Helper()
	ev, err := st.Evidence(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	for _, e := range ev {
		if e.ID == id {
			if e.ProbeID != string(probe) {
				t.Fatalf("claim cites %s evidence, want %s", e.ProbeID, probe)
			}
			return
		}
	}
	t.Fatalf("claim cites evidence %s, which is not stored", id)
}

// A node of another runway family is out of scope: the repair turn says
// so, and the repaired review is stored.
func TestModelTurn_RunwayRankingOfAnotherFamilyIsRepaired(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	bad := toolReply(func(body string) string {
		return wireReview{Ranking: append([]string{"xmin_held_by_session"},
			promptOpenNodes(t, body)...)}.json()
	})
	m := newFakeModel(t, bad, seqReview(t))
	c, _ := modelCoordinator(t, ctx, st, narrowColumnRunner(), m.client())
	inv := startAndRun(t, ctx, c, modelSeqTrigger("m6-model-scope"))
	if inv.Summary.Root != "column_narrower_than_sequence" || m.calls() != 2 {
		t.Fatalf("root %q calls %d", inv.Summary.Root, m.calls())
	}
	if !strings.Contains(m.body(t, 1), RejectOutOfScopeNode) ||
		!strings.Contains(m.body(t, 1), "xmin_held_by_session") {
		t.Fatalf("repair request does not name the out-of-scope node: %s", m.body(t, 1))
	}
	if r := inv.Summary.ModelRanking; r == nil || r.Nodes[0] != "column_narrower_than_sequence" {
		t.Fatalf("repaired ranking = %+v", r)
	}
}

// ownActionRunner is the narrow-column sequence with a pg_sage action in
// the window, so pg_sage's own change is an open alternative.
func ownActionRunner() *scriptedRunner {
	return narrowColumnRunner().script(probes.SageActions, rows(probes.SageActions,
		probes.Row{"id": int64(7), "action_type": "freeze", "outcome": "success",
			"age_s": 30.0}))
}

// A conclusive runway graph wins over a model that ranks another open
// hypothesis first: nothing the model said is kept.
func TestModelTurn_RunwayGraphRootWins(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(func(body string) string {
		open := promptOpenNodes(t, body)
		if len(open) < 2 {
			t.Errorf("want at least two open hypotheses, got %v", open)
			return wireReview{Ranking: open}.json()
		}
		open[0], open[1] = open[1], open[0]
		return wireReview{Ranking: open}.json()
	}))
	c, _ := modelCoordinator(t, ctx, st, ownActionRunner(), m.client())
	inv := startAndRun(t, ctx, c, modelSeqTrigger("m6-model-disagree"))
	if inv.Summary.Root != "column_narrower_than_sequence" {
		t.Fatalf("root = %q, want the graph's", inv.Summary.Root)
	}
	assertDeterministicOnly(t, inv)
}

// failedSequenceRunner: the sequence probe fails on both samples, so the
// graph is inconclusive and the model may ask for one probe.
func failedSequenceRunner() *scriptedRunner {
	failed := probes.Result{ProbeID: probes.SequenceRunwayProbe, Version: "v1",
		Status: probes.StatusError, Reason: "statement_timeout"}
	return newScriptedRunner().script(probes.SequenceRunwayProbe, failed, failed)
}

const trendWhy = "a longer trend window shows whether the sequence is consuming"

func TestModelProbe_RunwayTrendWithTypedWindow(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	first := toolReply(func(body string) string {
		return wireReview{Ranking: promptOpenNodes(t, body),
			NextProbe: nextProbe(string(probes.RunwayTrendsProbe),
				map[string]any{"window_seconds": 86400}, trendWhy)}.json()
	})
	final := toolReply(func(body string) string {
		return wireReview{Ranking: promptOpenNodes(t, body)}.json()
	})
	m := newFakeModel(t, first, final)
	runner := failedSequenceRunner()
	c, _ := modelCoordinator(t, ctx, st, runner, m.client())
	inv := startAndRun(t, ctx, c, modelSeqTrigger("m6-model-probe"))
	if inv.Summary.Root != "" || inv.State != StateConcluded && inv.State != StateInconclusive {
		t.Fatalf("investigation = %s root %q, want no root from failed probes",
			inv.State, inv.Summary.Root)
	}
	if !strings.Contains(m.body(t, 0), "- runway_trends: {} or {") {
		t.Fatalf("probe menu does not offer the runway trend window: %s", m.body(t, 0))
	}
	p := inv.Summary.ModelProbe
	if p == nil || p.ProbeID != string(probes.RunwayTrendsProbe) ||
		p.WindowSeconds != 86400 || p.Rationale != trendWhy || p.EvidenceID == "" {
		t.Fatalf("model probe = %+v", p)
	}
	assertEvidenceProbe(t, st, inv, p.EvidenceID, probes.RunwayTrendsProbe)
	if runner.calls[probes.RunwayTrendsProbe] != 2 || m.calls() != 2 {
		t.Fatalf("runway_trends ran %d times, model called %d times",
			runner.calls[probes.RunwayTrendsProbe], m.calls())
	}
}
