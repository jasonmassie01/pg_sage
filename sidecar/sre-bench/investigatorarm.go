package srebench

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
// RequireSupportCitation runs it under the Support-evidence rule
// (sre.investigator.require_support_citation); off is the Phase 1 bench
// the replay gates were set on, and the compare runs both.
type InvestigatorArm struct {
	Config                 LLMConfig
	RequireSupportCitation bool
}

// Name implements LiveArm.
func (InvestigatorArm) Name() string { return ArmInvestigator }

// Ready implements LiveArm, as for the LLM-on arm.
func (a InvestigatorArm) Ready() (bool, string) { return LLMArm{Config: a.Config}.Ready() }

// Investigate implements LiveArm: the fault's database also serves the
// plan-only EXPLAIN.
func (a InvestigatorArm) Investigate(ctx context.Context, e *Env, sc Scenario) (Trace,
	error) {
	client, tap, done, err := a.replayModel(sc)
	if err != nil {
		return Trace{}, err
	}
	defer done()
	cfg := &sre.InvestigatorConfig{Explainer: sre.NewStatementExplainer(e.Runner,
		benchStatementTexts(e.Pool)), RequireSupportCitation: a.RequireSupportCitation}
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
func (a InvestigatorArm) replayInvestigator() *sre.InvestigatorConfig {
	return &sre.InvestigatorConfig{RequireSupportCitation: a.RequireSupportCitation}
}

// UnmodeledPick is what the lift reads as the investigator's pick when it
// names a cause outside the graph: never a gold root.
const UnmodeledPick = "(unmodeled)"

// benchStatementTexts stands in for the sidecar's collector snapshot: the
// bench runs no collector, so it reads one statement's text from its own
// small fault database.
func benchStatementTexts(pool *pgxpool.Pool) sre.StatementTexts {
	return func(queryID int64) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var text string
		err := pool.QueryRow(ctx, `SELECT query FROM pg_stat_statements
			WHERE queryid = $1 AND dbid = (SELECT oid FROM pg_database
			      WHERE datname = current_database())
			ORDER BY calls DESC LIMIT 1`, queryID).Scan(&text)
		return text, err == nil && text != ""
	}
}
