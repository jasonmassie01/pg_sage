package srebench

import (
	"context"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
)

// ArmInvestigator is the tool-calling investigator (roadmap 2.1): the
// model plans read-only probes within the plan's budget and concludes
// with cited evidence; its root stays advisory (the bench grants no root
// authority).
const ArmInvestigator = "causal-graph+investigator"

// InvestigatorArm runs the coordinator with the tool-calling
// investigator against Config's model: the scripted fake investigator
// (CI default) or a live OpenAI-compatible endpoint.
type InvestigatorArm struct{ Config LLMConfig }

// Name implements LiveArm.
func (InvestigatorArm) Name() string { return ArmInvestigator }

// Ready implements LiveArm, as for the LLM-on arm.
func (a InvestigatorArm) Ready() (bool, string) { return LLMArm(a).Ready() }

// Investigate implements LiveArm: the fault's database also serves the
// plan-only EXPLAIN.
func (a InvestigatorArm) Investigate(ctx context.Context, e *Env, sc Scenario) (Trace,
	error) {
	client, tap, done, err := a.replayModel(sc)
	if err != nil {
		return Trace{}, err
	}
	defer done()
	cfg := &sre.InvestigatorConfig{Explainer: sre.NewStatementExplainer(e.Pool)}
	tr, err := e.investigate(ctx, sc, client, cfg)
	if err == nil && tr.Outcome.Model != nil {
		tr.Outcome.Model.Usage = tap.Usage()
	}
	return tr, err
}

func (a InvestigatorArm) replayModel(sc Scenario) (*llm.Client, *ModelTap, func(), error) {
	return tappedModel(a, a.Config, sc, func(id string) fakeServer {
		return NewFakeInvestigator(id)
	})
}

// replayInvestigator is the investigator of a replay: recorded evidence
// only, so no EXPLAIN against the bench database.
func (InvestigatorArm) replayInvestigator() *sre.InvestigatorConfig {
	return &sre.InvestigatorConfig{}
}

// UnmodeledPick is what the lift reads as the investigator's pick when it
// names a cause outside the graph: never a gold root.
const UnmodeledPick = "(unmodeled)"
