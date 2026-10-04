package sre

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// Investigator plans are data (roadmap 2.1, owner decision 4): one code
// path, a narrow plan for detector-triggered investigations and a broad
// one for operator-started and SLO-burn triage.

func TestInvestigatorPlans_DefaultsAreValidAndBroadIsBroader(t *testing.T) {
	plans := DefaultInvestigatorPlans()
	narrow, broad := plans[PlanNarrow], plans[PlanBroad]
	for _, p := range []InvestigatorPlan{narrow, broad} {
		if err := p.Validate(); err != nil {
			t.Fatalf("default plan %q invalid: %v", p.Name, err)
		}
	}
	if narrow.Name != PlanNarrow || broad.Name != PlanBroad {
		t.Fatalf("plan names = %q / %q", narrow.Name, broad.Name)
	}
	if broad.MaxSteps <= narrow.MaxSteps || broad.MaxProbes <= narrow.MaxProbes ||
		broad.Wall <= narrow.Wall || broad.MaxTokens <= narrow.MaxTokens {
		t.Fatalf("broad %+v is not broader than narrow %+v", broad, narrow)
	}
	if !slices.Contains(broad.Tools, ToolExplain) || !slices.Contains(broad.Tools, ToolRunProbe) {
		t.Fatalf("broad tools = %v, want probes and EXPLAIN", broad.Tools)
	}
	for _, p := range []InvestigatorPlan{narrow, broad} {
		for _, tool := range []string{ToolRunProbe, ToolGraphState, ToolFacts} {
			if !slices.Contains(p.Tools, tool) {
				t.Errorf("plan %s lacks %s", p.Name, tool)
			}
		}
	}
}

func TestInvestigatorPlans_SelectionIsByTrigger(t *testing.T) {
	cases := map[TriggerKind]string{TriggerOperator: PlanBroad, TriggerSLOBurn: PlanBroad,
		TriggerLock: PlanNarrow, TriggerConnections: PlanNarrow, TriggerWAL: PlanNarrow,
		TriggerPlan: PlanNarrow, TriggerCheckpoint: PlanNarrow, TriggerLWLock: PlanNarrow,
		TriggerSequence: PlanNarrow, "never_heard_of_it": PlanNarrow}
	for kind, want := range cases {
		if got := PlanForTrigger(kind); got != want {
			t.Errorf("PlanForTrigger(%s) = %s, want %s", kind, got, want)
		}
	}
}

func TestInvestigatorPlan_ValidateBounds(t *testing.T) {
	ok := DefaultInvestigatorPlans()[PlanBroad]
	cases := map[string]func(*InvestigatorPlan){
		"empty name":       func(p *InvestigatorPlan) { p.Name = "" },
		"zero steps":       func(p *InvestigatorPlan) { p.MaxSteps = 0 },
		"steps over":       func(p *InvestigatorPlan) { p.MaxSteps = CeilingInvestigatorSteps + 1 },
		"negative probes":  func(p *InvestigatorPlan) { p.MaxProbes = -1 },
		"probes over":      func(p *InvestigatorPlan) { p.MaxProbes = CeilingProbes + 1 },
		"zero wall":        func(p *InvestigatorPlan) { p.Wall = 0 },
		"wall over":        func(p *InvestigatorPlan) { p.Wall = CeilingActive + time.Second },
		"zero tokens":      func(p *InvestigatorPlan) { p.MaxTokens = 0 },
		"tokens over":      func(p *InvestigatorPlan) { p.MaxTokens = CeilingInvestigatorTokens + 1 },
		"zero step tokens": func(p *InvestigatorPlan) { p.StepTokens = 0 },
		"step over total":  func(p *InvestigatorPlan) { p.StepTokens = int(p.MaxTokens) + 1 },
		"unknown tool":     func(p *InvestigatorPlan) { p.Tools = append(p.Tools, "run_sql") },
		"no tools":         func(p *InvestigatorPlan) { p.Tools = nil },
		"duplicate tool": func(p *InvestigatorPlan) {
			p.Tools = append(p.Tools, ToolRunProbe)
		},
	}
	for name, mutate := range cases {
		p := ok
		p.Tools = append([]string(nil), ok.Tools...)
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Validate = %v, want ErrInvalidRequest", name, err)
		}
	}
	edge := ok
	edge.MaxSteps, edge.MaxProbes = CeilingInvestigatorSteps, CeilingProbes
	edge.Wall, edge.MaxTokens = CeilingActive, CeilingInvestigatorTokens
	if err := edge.Validate(); err != nil {
		t.Fatalf("a plan exactly at every ceiling was refused: %v", err)
	}
}

func TestInvestigatorConfig_ValidateNeedsBothPlans(t *testing.T) {
	if err := (InvestigatorConfig{}).Validate(); err != nil {
		t.Fatalf("zero config (defaults) refused: %v", err)
	}
	only := DefaultInvestigatorPlans()
	delete(only, PlanBroad)
	if err := (InvestigatorConfig{Plans: only}).Validate(); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("config without the broad plan: %v", err)
	}
	bad := DefaultInvestigatorPlans()
	p := bad[PlanNarrow]
	p.MaxSteps = 0
	bad[PlanNarrow] = p
	if err := (InvestigatorConfig{Plans: bad}).Validate(); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("config with an invalid plan: %v", err)
	}
	if err := (InvestigatorConfig{Protocol: "carrier_pigeon"}).Validate(); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("unknown protocol accepted: %v", err)
	}
}

func TestInvestigatorPlan_ProbeBudgetNeverPassesTheCeiling(t *testing.T) {
	p := DefaultInvestigatorPlans()[PlanBroad]
	l := DefaultLimits()
	cases := []struct{ used, want int }{
		{0, min(p.MaxProbes, l.MaxProbes)},
		{l.MaxProbes - 2, 2},
		{l.MaxProbes, 0},
		{l.MaxProbes + 3, 0},
	}
	for _, tc := range cases {
		if got := p.probeBudget(l, tc.used); got != tc.want {
			t.Errorf("probeBudget(used %d) = %d, want %d", tc.used, got, tc.want)
		}
	}
}

func TestInvestigatorPlan_ToolsAreReadOnlyByName(t *testing.T) {
	// The tool set is closed: every name the plans may offer is one of
	// the read-only investigator tools; none mutates.
	for _, p := range DefaultInvestigatorPlans() {
		for _, name := range p.Tools {
			if !readOnlyTools[name] {
				t.Errorf("plan %s offers %s, which is not a read-only investigator tool",
					p.Name, name)
			}
		}
	}
}
