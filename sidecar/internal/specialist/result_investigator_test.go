package specialist

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The tool-calling investigator's output in the contract (additive within
// v1): its verdict normalized against the graph, whether its root was
// adopted under the family's earned authority or stayed advisory (and
// why), its cited claims with the numbers their evidence holds, the reads
// it asked for that returned nothing usable, its run and a link to the
// redacted transcript. Model output is labelled and never changes the
// v1 fields' meaning.

const (
	graphRootNode = "idle_in_tx_holder"
	modelRootNode = "prepared_xact_holder"
)

func investigatorRun() *sre.InvestigatorRun {
	return &sre.InvestigatorRun{Label: sre.InvestigatorRunLabel, Plan: sre.PlanNarrow,
		Protocol: "native", ModelCalls: 4, ToolCalls: 5, Probes: 3, Tokens: 1200,
		Stop: "final", DroppedClaims: map[string]int{"uncited": 2, "duplicate": 1},
		Steps: []sre.InvestigatorStep{
			{Seq: 1, Call: 1, Tool: sre.ToolRunProbe,
				Args: json.RawMessage(`{"probe":"lock_graph"}`), Status: "ok",
				EvidenceID: ev1},
			{Seq: 2, Call: 2, Tool: sre.ToolRunProbe,
				Args:   json.RawMessage(`{"probe":"prepared_xacts"}`),
				Status: "no_privilege", EvidenceID: ev3,
				Note: "permission denied password=hunter2 on public.orders"},
			{Seq: 3, Call: 2, Tool: sre.ToolStatView,
				Args:   json.RawMessage(`{"view":"statements"}`),
				Status: "tool_error", Note: "the investigation's probe ceiling is reached"},
			{Seq: 4, Call: 3, Tool: sre.ToolExplain,
				Args: json.RawMessage(`{"queryid":42}`), Status: "rejected",
				Note: "invalid_args: queryid must be a non-zero integer"},
			{Seq: 5, Call: 3, Tool: sre.ToolRunProbe,
				Args:   json.RawMessage(`{"probe":"x'; DROP TABLE t;--"}`),
				Status: "rejected", Note: "not a catalog probe"},
			{Seq: 6, Call: 3, Status: "rejected", Note: "malformed reply"},
			{Seq: 7, Call: 4, Tool: sre.ToolSubmit, Status: "final"},
		}}
}

func investigatorNarrative() *sre.Narrative {
	return &sre.Narrative{Label: sre.NarrativeLabel, Claims: []sre.NarrativeClaim{
		{Text: "pid 4242 holds the lock 90 s; token=sk-abcdefghijklmnopqrstuv",
			EvidenceIDs: []sre.UUID{ev1}},
		{Text: "two old transactions on public.orders feed the queue",
			EvidenceIDs: []sre.UUID{ev1, ev2}}}}
}

// withInvestigator is the lock detail with an investigator conclusion.
func withInvestigator(mc *sre.ModelConclusion) sre.Detail {
	d := lockDetail()
	d.Investigation.Summary.ModelConclusion = mc
	d.Investigation.Summary.Investigator = investigatorRun()
	d.Investigation.Summary.Narrative = investigatorNarrative()
	return d
}

func conclusion(outcome, root, graphRoot, authority, reason string) *sre.ModelConclusion {
	return &sre.ModelConclusion{Label: sre.ModelConclusionLabel, Outcome: outcome,
		Root: root, GraphRoot: graphRoot, Authority: authority, Reason: reason}
}

// rootOn re-roots the lock detail's hypotheses on node (an adopted model
// root): the old root becomes contributing.
func rootOn(d *sre.Detail, node string) {
	d.Investigation.Summary.Root = node
	for i := range d.Hypotheses {
		switch d.Hypotheses[i].Node {
		case node:
			d.Hypotheses[i].Status = sre.HypothesisRoot
		case graphRootNode:
			d.Hypotheses[i].Status = sre.HypothesisContributing
		}
	}
}

func mapInvestigator(t *testing.T, d sre.Detail, keep bool) Result {
	t.Helper()
	return MapResult(Snapshot{Detail: d}, MapOptions{KeepIdentifiers: keep,
		RedactKey: []byte("k"), Now: created})
}

func TestInvestigator_AbsentWhenTheInvestigatorDidNotRun(t *testing.T) {
	d := lockDetail()
	// An M3 model turn's narrative alone is not the investigator.
	d.Investigation.Summary.Narrative = investigatorNarrative()
	r := mapInvestigator(t, d, true)
	if r.Investigator != nil {
		t.Fatalf("no investigator run, no section: %+v", r.Investigator)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"investigator"`) {
		t.Fatalf("a v1 result without an investigator carries no investigator key: %s", raw)
	}
}

func TestInvestigator_AgreeIsNotAnAdoption(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelAgreed, graphRootNode, graphRootNode,
		sre.ContestAdvisory, "the model agrees with the causal graph's root"))
	r := mapInvestigator(t, d, true)
	inv := r.Investigator
	if inv == nil || inv.Label != InvestigatorLabel || inv.Verdict != VerdictAgree ||
		inv.Root != graphRootNode || inv.GraphRoot != graphRootNode || inv.Cause != nil {
		t.Fatalf("agree: %+v", inv)
	}
	if inv.Adoption.Status != AdoptionNotApplicable ||
		inv.Adoption.Reason != "the model agrees with the causal graph's root" {
		t.Fatalf("agree adoption: %+v", inv.Adoption)
	}
	if r.RootCause.Source != "graph" || r.RootCause.Authority != "deterministic" {
		t.Fatalf("an agreeing model does not change the root's source: %+v", r.RootCause)
	}
}

func TestInvestigator_ContestAdvisoryKeepsTheGraphRoot(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelContested, modelRootNode, graphRootNode,
		sre.ContestAdvisory, "model-lift: 3/10 overrides right; not earned"))
	d.Investigation.Summary.ModelContest = &sre.ModelContest{Label: sre.ModelContestLabel,
		GraphRoot: graphRootNode, ModelRoot: modelRootNode, Authority: sre.ContestAdvisory,
		Reason: "model-lift: 3/10 overrides right; not earned"}
	r := mapInvestigator(t, d, true)
	inv := r.Investigator
	if inv.Verdict != VerdictContest || inv.Root != modelRootNode ||
		inv.GraphRoot != graphRootNode {
		t.Fatalf("contest: %+v", inv)
	}
	if inv.Adoption.Status != AdoptionAdvisory || inv.Adoption.Family != "lock_blocking" ||
		inv.Adoption.Reason != "model-lift: 3/10 overrides right; not earned" {
		t.Fatalf("advisory contest names the family authority and why: %+v", inv.Adoption)
	}
	if r.RootCause.Node != graphRootNode || r.RootCause.Source != "graph" ||
		r.RootCause.Authority != "deterministic" || r.ModelContest == nil ||
		r.ModelContest.Authority != "advisory" {
		t.Fatalf("v1 fields unchanged: %+v %+v", r.RootCause, r.ModelContest)
	}
}

func TestInvestigator_ContestAdoptedUnderEarnedAuthority(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelContested, modelRootNode, graphRootNode,
		sre.ContestAdopted, "model-lift: 16/16 overrides right on the held-out bench"))
	d.Investigation.Summary.ModelContest = &sre.ModelContest{Label: sre.ModelContestLabel,
		GraphRoot: graphRootNode, ModelRoot: modelRootNode, Authority: sre.ContestAdopted,
		Reason: "model-lift: 16/16 overrides right on the held-out bench"}
	d.Hypotheses[2].Node = modelRootNode // the unproven prepared-xact hypothesis
	rootOn(&d, modelRootNode)
	r := mapInvestigator(t, d, true)
	inv := r.Investigator
	if inv.Verdict != VerdictContest || inv.Adoption.Status != AdoptionAdopted ||
		inv.Adoption.Family != "lock_blocking" ||
		!strings.Contains(inv.Adoption.Reason, "16/16") {
		t.Fatalf("adopted contest: %+v", inv)
	}
	if r.RootCause.Node != modelRootNode || r.RootCause.Source != "model" ||
		r.RootCause.Authority != "model_earned" {
		t.Fatalf("adopted root: %+v", r.RootCause)
	}
}

// A conclusion of an inconclusive graph adopted under earned authority is
// a model root: v1 documents source=model, authority=model_earned for it.
func TestInvestigator_ConcludeAdoptedIsAModelRoot(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelConcluded, modelRootNode, "",
		sre.ContestAdopted, "model-lift: 14/15 conclusions right on the held-out bench"))
	d.Hypotheses[2].Node = modelRootNode
	rootOn(&d, modelRootNode)
	r := mapInvestigator(t, d, true)
	inv := r.Investigator
	if inv.Verdict != VerdictConclude || inv.Root != modelRootNode || inv.GraphRoot != "" ||
		inv.Adoption.Status != AdoptionAdopted || inv.Adoption.Family != "lock_blocking" {
		t.Fatalf("adopted conclusion: %+v", inv)
	}
	if r.RootCause == nil || r.RootCause.Node != modelRootNode ||
		r.RootCause.Source != "model" || r.RootCause.Authority != "model_earned" {
		t.Fatalf("an adopted conclusion is a model root: %+v", r.RootCause)
	}
}

func TestInvestigator_ConcludeAdvisoryLeavesTheGraphInconclusive(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelConcluded, modelRootNode, "",
		sre.ContestAdvisory, "no root authority is configured, so model roots stay "+
			"advisory (L1)"))
	d.Investigation.State = sre.StateInconclusive
	d.Investigation.Summary.Conclusive, d.Investigation.Summary.Root = false, ""
	d.Hypotheses[0].Status = sre.HypothesisUnproven
	r := mapInvestigator(t, d, true)
	inv := r.Investigator
	if inv.Verdict != VerdictConclude || inv.Adoption.Status != AdoptionAdvisory ||
		!strings.Contains(inv.Adoption.Reason, "no root authority") {
		t.Fatalf("advisory conclusion: %+v", inv)
	}
	if r.RootCause != nil || r.Outcome != "inconclusive" {
		t.Fatalf("an advisory conclusion is never the root: %+v %s", r.RootCause, r.Outcome)
	}
}

func TestInvestigator_UnmodeledCauseIsAlwaysAdvisory(t *testing.T) {
	mc := conclusion(sre.ModelUnmodeled, "", graphRootNode, sre.ContestAdvisory,
		"an unmodeled cause has no graph node, so it stays advisory (L1)")
	mc.Cause = &sre.UnmodeledCause{Label: "volume latency on public.orders storage",
		Mechanism: "fsync waits climb; see postgres://u:p@db.internal/orders"}
	r := mapInvestigator(t, withInvestigator(mc), true)
	inv := r.Investigator
	if inv.Verdict != VerdictUnmodeled || inv.Root != "" || inv.Cause == nil ||
		inv.Cause.Label != "volume latency on public.orders storage" ||
		inv.Adoption.Status != AdoptionAdvisory || inv.Adoption.Family != "" {
		t.Fatalf("unmodeled: %+v", inv)
	}
	if strings.Contains(inv.Cause.Mechanism, "u:p@") {
		t.Fatalf("connection URI in the cause: %s", inv.Cause.Mechanism)
	}
	if r.RootCause.Source != "graph" {
		t.Fatalf("an unmodeled cause never becomes the root: %+v", r.RootCause)
	}
}

func TestInvestigator_InconclusiveAndNoAnswer(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelInconclusive, "", graphRootNode,
		sre.ContestAdvisory, "the model did not conclude (uncited)"))
	inv := mapInvestigator(t, d, true).Investigator
	if inv.Verdict != VerdictInconclusive || inv.Adoption.Status != AdoptionNotApplicable ||
		inv.Adoption.Reason != "the model did not conclude (uncited)" {
		t.Fatalf("inconclusive: %+v", inv)
	}
	d = withInvestigator(nil)
	d.Investigation.Summary.Investigator.Stop = "budget_exhausted"
	inv = mapInvestigator(t, d, true).Investigator
	if inv == nil || inv.Verdict != VerdictNoAnswer || inv.Root != "" ||
		inv.Adoption.Status != AdoptionNotApplicable ||
		!strings.Contains(inv.Adoption.Reason, "budget_exhausted") ||
		inv.Run.Stop != "budget_exhausted" {
		t.Fatalf("a run without an answer: %+v", inv)
	}
}

func TestInvestigator_UnknownOutcomeIsNotPassedThrough(t *testing.T) {
	mc := conclusion("overruled", graphRootNode, graphRootNode, sre.ContestAdvisory, "x")
	inv := mapInvestigator(t, withInvestigator(mc), true).Investigator
	if inv.Verdict != VerdictInconclusive || inv.Adoption.Status != AdoptionNotApplicable {
		t.Fatalf("an outcome the contract does not know maps to inconclusive: %+v", inv)
	}
}

func TestInvestigator_ClaimsCiteEvidenceWithNumbers(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelAgreed, graphRootNode, graphRootNode,
		sre.ContestAdvisory, "agrees"))
	inv := mapInvestigator(t, d, true).Investigator
	if len(inv.Claims) != 2 || inv.DroppedClaims != 3 {
		t.Fatalf("claims %+v dropped %d", inv.Claims, inv.DroppedClaims)
	}
	first := inv.Claims[0]
	if len(first.Evidence) != 1 || first.Evidence[0].EvidenceID != string(ev1) ||
		first.Evidence[0].Numbers["blocker_pid"] != 4242 ||
		first.Evidence[0].Numbers["blocker_xact_age_s"] != 90.5 {
		t.Fatalf("claim citation carries its evidence's numbers: %+v", first)
	}
	second := inv.Claims[1]
	if len(second.Evidence) != 2 || second.Evidence[1].EvidenceID != string(ev2) ||
		second.Evidence[1].Numbers["rows"] != 2 {
		t.Fatalf("second claim: %+v", second)
	}
	if first.Evidence[0].Text != "" {
		t.Fatalf("a claim citation's text is the claim, not repeated: %+v", first.Evidence)
	}
}

func TestInvestigator_MissingEvidenceItAskedFor(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelAgreed, graphRootNode, graphRootNode,
		sre.ContestAdvisory, "agrees"))
	inv := mapInvestigator(t, d, true).Investigator
	want := map[string]string{"prepared_xacts": "no_privilege",
		sre.ToolStatView + ":statements": "tool_error", sre.ToolExplain: "rejected",
		sre.ToolRunProbe: "rejected"}
	if len(inv.MissingEvidence) != len(want) {
		t.Fatalf("missing evidence %+v", inv.MissingEvidence)
	}
	for _, m := range inv.MissingEvidence {
		if want[m.ProbeID] != m.Status || m.Source != "investigator" || m.Reason == "" {
			t.Errorf("unexpected missing evidence %+v", m)
		}
		if strings.Contains(m.ProbeID, "DROP") || strings.Contains(m.Reason, "hunter2") {
			t.Errorf("model text leaked into missing evidence: %+v", m)
		}
	}
}

func TestInvestigator_RunAndTranscriptLink(t *testing.T) {
	d := withInvestigator(conclusion(sre.ModelAgreed, graphRootNode, graphRootNode,
		sre.ContestAdvisory, "agrees"))
	inv := mapInvestigator(t, d, true).Investigator
	if inv.Run.Plan != sre.PlanNarrow || inv.Run.Protocol != "native" ||
		inv.Run.Stop != "final" || inv.Run.ModelCalls != 4 || inv.Run.ToolCalls != 5 ||
		inv.Run.Probes != 3 {
		t.Fatalf("run %+v", inv.Run)
	}
	tr := inv.Transcript
	if tr == nil || tr.Href != BasePath+"/databases/orders/investigations/"+string(inv0())+
		"/transcript" || tr.MCPTool != "specialist_investigation_transcript" ||
		tr.Schema != sre.TranscriptSchema || !tr.Redacted {
		t.Fatalf("transcript link %+v", tr)
	}
	d.Investigation.Summary.Investigator = nil
	inv = mapInvestigator(t, d, true).Investigator
	if inv == nil || inv.Transcript != nil || inv.Verdict != VerdictAgree {
		t.Fatalf("a conclusion without a stored run has no transcript link: %+v", inv)
	}
}

func inv0() sre.UUID { return inv }

func TestInvestigator_RedactionPreserved(t *testing.T) {
	mc := conclusion(sre.ModelContested, modelRootNode, graphRootNode, sre.ContestAdvisory,
		"ledger says no for bob@example.com password=hunter2")
	d := withInvestigator(mc)
	for _, keep := range []bool{true, false} {
		r := mapInvestigator(t, d, keep)
		raw, err := json.Marshal(r.Investigator)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"hunter2", "bob@example.com",
			"sk-abcdefghijklmnopqrstuv"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("keep=%t: %q leaked: %s", keep, leak, raw)
			}
		}
		claim := r.Investigator.Claims[1].Text
		if keep != strings.Contains(claim, "public.orders") {
			t.Fatalf("keep=%t identifiers in claims follow keep_identifiers: %s", keep,
				claim)
		}
		if !keep && !strings.Contains(claim, "id_") {
			t.Fatalf("hashed identifier expected: %s", claim)
		}
	}
}
