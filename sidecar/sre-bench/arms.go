package srebench

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Arm names, as the report lists them.
const (
	// ArmCausalGraph is the deterministic investigator with the LLM off.
	ArmCausalGraph = "causal-graph"
	// ArmLLM is the investigator with its model turn on: the product path.
	ArmLLM = "causal-graph+llm"
	// ArmAlwaysEscalate abstains on every run.
	ArmAlwaysEscalate = "always-escalate"
	// ArmRulesOnly is a first-match rule list per family.
	ArmRulesOnly = "rules-only"
)

// Trace is what a live arm's investigation left behind: its outcome and
// the probe results it stored, which derived arms read. scope lists
// evidence references outside the investigation (graded on replay).
type Trace struct {
	Outcome  Outcome
	Evidence []probes.Result
	scope    []string
}

// LiveArm investigates a fault present on the database. Each ready live
// arm gets its own injection of every scenario. An arm that is not ready
// is still listed in the report, with its reason, and never run.
type LiveArm interface {
	Name() string
	Ready() (bool, string)
	Investigate(ctx context.Context, e *Env, sc Scenario) (Trace, error)
}

// DerivedArm diagnoses from the first ready live arm's evidence, without
// probes or sessions of its own.
type DerivedArm interface {
	Name() string
	Derive(sc Scenario, t Trace) Outcome
}

// RunConfig is the arms a run scores side by side and its repeat count.
type RunConfig struct {
	Repeats int
	Live    []LiveArm
	Derived []DerivedArm
}

// DefaultConfig is every arm of PGIncidentBench v1: the causal graph
// (LLM off), the LLM-on arm and the tool-calling investigator with llm's
// model, always-escalate and rules-only.
func DefaultConfig(repeats int, llm LLMConfig) RunConfig {
	return RunConfig{Repeats: repeats, Live: []LiveArm{CausalGraph{}, LLMArm{Config: llm},
		InvestigatorArm{Config: llm}}, Derived: []DerivedArm{AlwaysEscalate{}, RulesOnly{}}}
}

// ArmNames lists the live arms (ready or not), then the derived arms.
func (c RunConfig) ArmNames() []string {
	out := make([]string, 0, len(c.Live)+len(c.Derived))
	for _, a := range c.Live {
		out = append(out, a.Name())
	}
	for _, a := range c.Derived {
		out = append(out, a.Name())
	}
	return out
}

// Pending maps each live arm that is not ready to the reason.
func (c RunConfig) Pending() map[string]string {
	out := map[string]string{}
	for _, a := range c.Live {
		if ok, why := a.Ready(); !ok {
			out[a.Name()] = why
		}
	}
	return out
}

// Gated lists the arms whose failed gates fail the bench: every live arm.
// A pending arm's gates are "not evaluated" until it is ready.
func (c RunConfig) Gated() []string {
	out := make([]string, 0, len(c.Live))
	for _, a := range c.Live {
		out = append(out, a.Name())
	}
	return out
}

// CausalGraph is the deterministic investigator (probe plan, causal
// graph, no LLM) run through the real coordinator and store.
type CausalGraph struct{}

// Name implements LiveArm.
func (CausalGraph) Name() string { return ArmCausalGraph }

// Ready implements LiveArm.
func (CausalGraph) Ready() (bool, string) { return true, "" }

// Investigate implements LiveArm: no model.
func (CausalGraph) Investigate(ctx context.Context, e *Env, sc Scenario) (Trace, error) {
	return e.investigate(ctx, sc, nil, nil)
}

// errNotReady is returned by an arm asked to run before it is ready.
var errNotReady = errors.New("arm is not ready")

// LLMArm is the investigator with its model turn on (sre.llm.enabled),
// against Config's model: the fake adversarial model (CI default) or a
// live OpenAI-compatible endpoint.
type LLMArm struct{ Config LLMConfig }

// Name implements LiveArm.
func (LLMArm) Name() string { return ArmLLM }

// Ready implements LiveArm: the fake model always; a live endpoint once
// it has a URL and a model.
func (a LLMArm) Ready() (bool, string) {
	switch {
	case a.Config.Mode == LLMFake:
		return true, ""
	case a.Config.Mode != LLMLive:
		return false, fmt.Sprintf("unknown model mode %q", a.Config.Mode)
	case a.Config.URL == "" || a.Config.Model == "":
		return false, fmt.Sprintf("a live model needs %s and %s", EnvLLMURL, EnvLLMModel)
	}
	return true, ""
}

// Investigate implements LiveArm: the run's model is a fresh fake seeded
// by the scenario id, or the live endpoint, behind a model tap that
// records the run's token usage.
func (a LLMArm) Investigate(ctx context.Context, e *Env, sc Scenario) (Trace, error) {
	client, tap, done, err := a.tappedClient(sc)
	if err != nil {
		return Trace{}, err
	}
	defer done()
	tr, err := e.investigate(ctx, sc, client, nil)
	if err == nil && tr.Outcome.Model != nil {
		tr.Outcome.Model.Usage = tap.Usage()
	}
	return tr, err
}

// AlwaysEscalate abstains on every run: it is never wrong and never
// useful, which is what Safe Pass alone cannot tell apart.
type AlwaysEscalate struct{}

// Name implements DerivedArm.
func (AlwaysEscalate) Name() string { return ArmAlwaysEscalate }

// Derive implements DerivedArm.
func (AlwaysEscalate) Derive(Scenario, Trace) Outcome {
	return Outcome{State: sre.StateInconclusive}
}
