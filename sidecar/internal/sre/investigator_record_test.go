package sre

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The stored investigator output is validated again before any store
// I/O: a conclusion's authority must match the persisted root, an
// unmodeled cause carries no graph node, and the transcript is bounded
// and cites only well-formed evidence ids.

func lockHypotheses() []HypothesisRecord {
	return []HypothesisRecord{
		{Node: "idle_in_tx_holder", Status: HypothesisRoot},
		{Node: "ddl_lock_queue", Status: HypothesisContributing},
		{Node: "prepared_xact_holder", Status: HypothesisRuledOut},
	}
}

func validRun() *InvestigatorRun {
	return &InvestigatorRun{Label: InvestigatorRunLabel, Plan: PlanNarrow,
		Budget:   InvestigatorBudget{MaxSteps: 5, MaxProbes: 3, WallMS: 40000, MaxTokens: 24000},
		Protocol: "native", Stop: "final", ModelCalls: 2, ToolCalls: 1, Probes: 1,
		Steps: []InvestigatorStep{{Seq: 1, Call: 1, Tool: ToolRunProbe, Status: "ok",
			EvidenceID: NewUUID(), Digest: strings.Repeat("ab", 32), Cost: 1}}}
}

func TestModelConclusion_ValidateMatchesTheRoot(t *testing.T) {
	hs := lockHypotheses()
	ok := []struct {
		mc        ModelConclusion
		root      string
		concluded bool
	}{
		{ModelConclusion{Outcome: ModelAgreed, Root: "idle_in_tx_holder",
			GraphRoot: "idle_in_tx_holder", Authority: ContestAdvisory}, "idle_in_tx_holder", true},
		{ModelConclusion{Outcome: ModelContested, Root: "ddl_lock_queue",
			GraphRoot: "idle_in_tx_holder", Authority: ContestAdvisory}, "idle_in_tx_holder", true},
		{ModelConclusion{Outcome: ModelContested, Root: "ddl_lock_queue",
			GraphRoot: "idle_in_tx_holder", Authority: ContestAdopted}, "ddl_lock_queue", true},
		{ModelConclusion{Outcome: ModelConcluded, Root: "ddl_lock_queue",
			Authority: ContestAdvisory}, "", false},
		{ModelConclusion{Outcome: ModelConcluded, Root: "ddl_lock_queue",
			Authority: ContestAdopted}, "ddl_lock_queue", true},
		{ModelConclusion{Outcome: ModelUnmodeled, GraphRoot: "idle_in_tx_holder",
			Cause:     &UnmodeledCause{Label: "deploy", Mechanism: "a job paused"},
			Authority: ContestAdvisory}, "idle_in_tx_holder", true},
		{ModelConclusion{Outcome: ModelInconclusive, Authority: ContestAdvisory}, "", false},
	}
	for i, tc := range ok {
		mc := tc.mc
		mc.Label, mc.Reason = ModelConclusionLabel, "why"
		if err := mc.validate(hs, tc.root, tc.concluded); err != nil {
			t.Errorf("valid case %d (%+v) refused: %v", i, mc, err)
		}
	}
	bad := map[string]func(*ModelConclusion){
		"no label":             func(m *ModelConclusion) { m.Label = "" },
		"unknown outcome":      func(m *ModelConclusion) { m.Outcome = "vibes" },
		"unknown authority":    func(m *ModelConclusion) { m.Authority = "sovereign" },
		"adopted not root":     func(m *ModelConclusion) { m.Authority = ContestAdopted },
		"unknown node":         func(m *ModelConclusion) { m.Root = "cosmic_rays" },
		"empty reason":         func(m *ModelConclusion) { m.Reason = " " },
		"long reason":          func(m *ModelConclusion) { m.Reason = strings.Repeat("r", 600) },
		"cause on a contest":   func(m *ModelConclusion) { m.Cause = &UnmodeledCause{Label: "x"} },
		"control characters":   func(m *ModelConclusion) { m.Reason = "a\x00b" },
		"advisory equals root": func(m *ModelConclusion) { m.Root = "idle_in_tx_holder" },
	}
	for name, mutate := range bad {
		mc := ModelConclusion{Label: ModelConclusionLabel, Outcome: ModelContested,
			Root: "ddl_lock_queue", GraphRoot: "idle_in_tx_holder",
			Authority: ContestAdvisory, Reason: "why"}
		mutate(&mc)
		if err := mc.validate(hs, "idle_in_tx_holder", true); !errors.Is(err,
			ErrInvalidRequest) {
			t.Errorf("%s: validate = %v, want ErrInvalidRequest", name, err)
		}
	}
	var none *ModelConclusion
	if err := none.validate(hs, "", false); err != nil {
		t.Fatalf("nil conclusion: %v", err)
	}
}

func TestModelConclusion_UnmodeledNeedsACauseAndNoNode(t *testing.T) {
	hs := lockHypotheses()
	for name, mc := range map[string]ModelConclusion{
		"no cause": {Outcome: ModelUnmodeled},
		"with a node": {Outcome: ModelUnmodeled, Root: "ddl_lock_queue",
			Cause: &UnmodeledCause{Label: "x", Mechanism: "y"}},
		"adopted": {Outcome: ModelUnmodeled, Authority: ContestAdopted,
			Cause: &UnmodeledCause{Label: "x", Mechanism: "y"}},
	} {
		mc.Label, mc.Reason = ModelConclusionLabel, "why"
		if mc.Authority == "" {
			mc.Authority = ContestAdvisory
		}
		if err := mc.validate(hs, "idle_in_tx_holder", true); !errors.Is(err,
			ErrInvalidRequest) {
			t.Errorf("%s: validate = %v", name, err)
		}
	}
}

func TestInvestigatorRun_ValidateBounds(t *testing.T) {
	if err := validRun().validate(); err != nil {
		t.Fatalf("valid run refused: %v", err)
	}
	bad := map[string]func(*InvestigatorRun){
		"no label":      func(r *InvestigatorRun) { r.Label = "" },
		"unknown plan":  func(r *InvestigatorRun) { r.Plan = "galaxy_brain" },
		"bad evidence":  func(r *InvestigatorRun) { r.Steps[0].EvidenceID = "E7" },
		"bad digest":    func(r *InvestigatorRun) { r.Steps[0].Digest = "xyz" },
		"empty stop":    func(r *InvestigatorRun) { r.Stop = "" },
		"negative cost": func(r *InvestigatorRun) { r.Steps[0].Cost = -1 },
		"long model plan": func(r *InvestigatorRun) {
			r.ModelPlan = strings.Repeat("p", maxModelPlanRunes+1)
		},
		"too many steps": func(r *InvestigatorRun) {
			for len(r.Steps) <= MaxInvestigatorSteps {
				r.Steps = append(r.Steps, InvestigatorStep{Tool: ToolGraphState, Status: "ok"})
			}
		},
		"long note": func(r *InvestigatorRun) {
			r.Steps[0].Note = strings.Repeat("n", 400)
		},
		"oversized args": func(r *InvestigatorRun) {
			r.Steps[0].Args = []byte(`{"x":"` + strings.Repeat("a", maxStepArgsBytes) + `"}`)
		},
	}
	for name, mutate := range bad {
		r := validRun()
		mutate(r)
		if err := r.validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: validate = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestSummary_ModelRefsIncludeTheTranscriptEvidence(t *testing.T) {
	run := validRun()
	s := Summary{Investigator: run}
	refs := s.modelRefs()
	if len(refs) != 1 || refs[0] != run.Steps[0].EvidenceID {
		t.Fatalf("refs = %v, want the transcript's evidence id", refs)
	}
}

func TestModelOutcome_InvestigatorFieldsAreApplied(t *testing.T) {
	out := modelOutcome{run: validRun(), conclusion: &ModelConclusion{Outcome: ModelAgreed}}
	if out.empty() {
		t.Fatal("an outcome with a transcript reported empty")
	}
	var s Summary
	out.apply(&s)
	if s.Investigator == nil || s.ModelConclusion == nil {
		t.Fatalf("summary = %+v", s)
	}
}

func TestReplayExport_ModelContestIsNamedAndTheGraphRootKept(t *testing.T) {
	inv := Investigation{ID: NewUUID(), State: StateConcluded, TriggerKind: TriggerLock,
		CreatedAt: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC),
		Summary: Summary{Family: "lock_blocking", Root: "ddl_lock_queue", Conclusive: true,
			ModelConclusion: &ModelConclusion{Label: ModelConclusionLabel,
				Outcome: ModelContested, Root: "ddl_lock_queue", GraphRoot: "idle_in_tx_holder",
				Authority: ContestAdopted, Reason: "earned"},
			ModelContest: &ModelContest{Label: ModelContestLabel, GraphRoot: "idle_in_tx_holder",
				ModelRoot: "ddl_lock_queue", Authority: ContestAdopted, Reason: "earned"}}}
	if got := graphRootOf(inv.Summary); got != "idle_in_tx_holder" {
		t.Fatalf("graph root = %s, want the graph's own idle_in_tx_holder", got)
	}
	concluded := inv
	concluded.Summary.ModelContest = nil
	concluded.Summary.ModelConclusion = &ModelConclusion{Label: ModelConclusionLabel,
		Outcome: ModelConcluded, Root: "ddl_lock_queue", Authority: ContestAdopted,
		Reason: "earned"}
	if got := graphRootOf(concluded.Summary); got != "" {
		t.Fatalf("graph root of an adopted conclusion = %q, want the graph's inconclusive", got)
	}
	ev := fixtureEvidence(t, lockGraphResult(inv.CreatedAt), explainResult(inv.CreatedAt))
	out := &Outcome{Verdict: OutcomeRefuted, ActualNode: "idle_in_tx_holder",
		RecordedAt: inv.CreatedAt.Add(time.Hour)}
	exp, err := BuildReplayCase(inv, ev, out, ReplayExportOptions{salt: []byte("k")})
	if err != nil {
		t.Fatalf("BuildReplayCase: %v", err)
	}
	if !containsAll(strings.Join(exp.Case.Tags, ","), "contested", ModelContestTag) {
		t.Fatalf("tags = %v, want the model contest tagged", exp.Case.Tags)
	}
	if exp.ModelContest == nil || exp.ModelContest.ModelRoot != "ddl_lock_queue" ||
		exp.ModelContest.Authority != ContestAdopted {
		t.Fatalf("model contest = %+v", exp.ModelContest)
	}
	for _, o := range exp.Case.Observations {
		if o.Probe == ExplainProbeID {
			t.Fatal("a non-catalog explain observation was exported into a replay case")
		}
	}
}

func lockGraphResult(at time.Time) probes.Result {
	r := idleChainRunner().Run(context.Background(), probes.LockGraph, probes.Args{})
	r.ObservedAt = at
	return r
}

func explainResult(at time.Time) probes.Result {
	return probes.Result{ProbeID: ExplainProbeID, Version: "v1", Status: probes.StatusOK,
		ObservedAt: at.Add(time.Second), Rows: []probes.Row{{"node_type": "Seq Scan",
			"relation": "public.orders", "total_cost": 12.5}}}
}
