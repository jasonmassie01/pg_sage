package sre

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// Investigator plans (roadmap 2.1, owner decision 4): how much one
// tool-calling investigation may do is data, not a code path. Detector-
// triggered investigations use the narrow plan; operator-started and
// SLO-burn triage the broad one. Every plan stays under hard ceilings
// that configuration cannot raise; the probe budget also stays under the
// investigation's 12-probe ceiling, the deterministic plan's probes
// included.

// Plan names.
const (
	PlanNarrow = "narrow"
	PlanBroad  = "broad"
)

// Investigator tool names: the closed, read-only tool set.
const (
	ToolRunProbe   = "run_probe"
	ToolStatView   = "read_stat_view"
	ToolExplain    = "explain_statement"
	ToolGraphState = "graph_state"
	ToolFacts      = "confirmed_facts"
	ToolSubmit     = "submit_conclusion"
)

// readOnlyTools are every tool a plan may offer; none can mutate.
var readOnlyTools = map[string]bool{ToolRunProbe: true, ToolStatView: true,
	ToolExplain: true, ToolGraphState: true, ToolFacts: true}

// Investigator ceilings. The active-time ceiling (120 s) and the probe
// ceiling (12) are the investigation's own.
const (
	CeilingInvestigatorSteps  = 12
	CeilingInvestigatorTokens = 64000
)

// InvestigatorPlan bounds one tool-calling investigation: model steps,
// probe calls (beyond the deterministic plan's), wall clock, tokens
// (prompt, completion and reasoning, every step), each step's completion
// cap, and the tools offered: Tools always, TriggerTools besides them for
// one trigger kind.
type InvestigatorPlan struct {
	Name         string
	MaxSteps     int
	MaxProbes    int
	Wall         time.Duration
	MaxTokens    int64
	StepTokens   int
	Tools        []string
	TriggerTools map[TriggerKind][]string
}

var defaultPlans = map[string]InvestigatorPlan{
	PlanNarrow: {Name: PlanNarrow, MaxSteps: 5, MaxProbes: 3, Wall: 45 * time.Second,
		MaxTokens: 32000, StepTokens: 1024,
		Tools: []string{ToolRunProbe, ToolStatView, ToolGraphState, ToolFacts},
		// A plan regression is about one statement's plan: the narrow plan
		// may read it (plan-only EXPLAIN) without a broader budget.
		TriggerTools: map[TriggerKind][]string{TriggerPlan: {ToolExplain}}},
	PlanBroad: {Name: PlanBroad, MaxSteps: 10, MaxProbes: 6, Wall: 90 * time.Second,
		MaxTokens: 64000, StepTokens: 1024,
		Tools: []string{ToolRunProbe, ToolStatView, ToolExplain, ToolGraphState, ToolFacts}},
}

// triggerPlans names the plan of each trigger kind; any other is narrow.
var triggerPlans = map[TriggerKind]string{TriggerOperator: PlanBroad,
	TriggerSLOBurn: PlanBroad}

// DefaultInvestigatorPlans returns a copy of the default plans.
func DefaultInvestigatorPlans() map[string]InvestigatorPlan {
	out := make(map[string]InvestigatorPlan, len(defaultPlans))
	for k, p := range defaultPlans {
		p.Tools = append([]string(nil), p.Tools...)
		extra := make(map[TriggerKind][]string, len(p.TriggerTools))
		for kind, tools := range p.TriggerTools {
			extra[kind] = append([]string(nil), tools...)
		}
		p.TriggerTools = extra
		out[k] = p
	}
	return out
}

// ToolsFor is a fresh list of the tools the plan offers for a trigger kind.
func (p InvestigatorPlan) ToolsFor(kind TriggerKind) []string {
	out := append([]string(nil), p.Tools...)
	return append(out, p.TriggerTools[kind]...)
}

// PlanForTrigger is the plan name of a trigger kind.
func PlanForTrigger(kind TriggerKind) string {
	if p, ok := triggerPlans[kind]; ok {
		return p
	}
	return PlanNarrow
}

// Validate checks a plan against the ceilings.
func (p InvestigatorPlan) Validate() error {
	checks := []struct {
		ok      bool
		problem string
	}{
		{p.Name != "", "a name"},
		{p.MaxSteps >= 1 && p.MaxSteps <= CeilingInvestigatorSteps, "steps in [1, 12]"},
		{p.MaxProbes >= 0 && p.MaxProbes <= CeilingProbes, "probes in [0, 12]"},
		{p.Wall > 0 && p.Wall <= CeilingActive, "a wall clock in (0, 120s]"},
		{p.MaxTokens > 0 && p.MaxTokens <= CeilingInvestigatorTokens,
			"tokens in [1, 64000]"},
		{p.StepTokens > 0 && int64(p.StepTokens) <= p.MaxTokens,
			"step tokens in [1, max tokens]"},
		{len(p.Tools) > 0, "at least one tool"},
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%w: investigator plan %q needs %s", ErrInvalidRequest, p.Name,
				c.problem)
		}
	}
	if err := p.checkTools(p.Tools); err != nil {
		return err
	}
	for kind := range p.TriggerTools {
		if err := p.checkTools(p.ToolsFor(kind)); err != nil {
			return err
		}
	}
	return nil
}

// checkTools refuses an unknown (or the final) tool and a repeated one.
func (p InvestigatorPlan) checkTools(tools []string) error {
	seen := map[string]bool{}
	for _, t := range tools {
		if !readOnlyTools[t] || seen[t] {
			return fmt.Errorf("%w: investigator plan %q: tool %q is unknown or repeated",
				ErrInvalidRequest, p.Name, t)
		}
		seen[t] = true
	}
	return nil
}

// probeBudget is the probes the plan may still run: its own budget,
// within what the investigation's ceiling leaves after used probes.
func (p InvestigatorPlan) probeBudget(l Limits, used int) int {
	return max(0, min(p.MaxProbes, l.MaxProbes-used))
}

// InvestigatorConfig turns the model turn into the tool-calling
// investigator. Plans nil uses the defaults; Explainer nil offers no
// EXPLAIN; Protocol "" is auto (native tool calls, JSON actions when the
// provider refuses them).
type InvestigatorConfig struct {
	Plans     map[string]InvestigatorPlan
	Explainer Explainer
	Protocol  agentloop.Protocol
}

// Validate checks the plans and the protocol.
func (c InvestigatorConfig) Validate() error {
	switch c.Protocol {
	case "", agentloop.ProtocolAuto, agentloop.ProtocolNative, agentloop.ProtocolJSON:
	default:
		return fmt.Errorf("%w: investigator protocol %q", ErrInvalidRequest, c.Protocol)
	}
	plans := c.plans()
	for _, name := range []string{PlanNarrow, PlanBroad} {
		p, ok := plans[name]
		if !ok {
			return fmt.Errorf("%w: investigator plan %q is missing", ErrInvalidRequest, name)
		}
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c InvestigatorConfig) plans() map[string]InvestigatorPlan {
	if c.Plans == nil {
		return DefaultInvestigatorPlans()
	}
	return c.Plans
}

func (c InvestigatorConfig) protocol() agentloop.Protocol {
	if c.Protocol == "" {
		return agentloop.ProtocolAuto
	}
	return c.Protocol
}
