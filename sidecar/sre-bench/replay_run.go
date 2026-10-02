package srebench

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// The replay arm of PGIncidentBench (AI-SRE-SPEC §12 source 1): each
// recorded case runs through the real coordinator and store, with the
// replay runner in place of the probe runner and no wait between
// samples, once per arm (the causal graph; the LLM-on arm with its fake
// or live model). The grader then reads what came out: the runner's
// refused calls, the database before and after, and every surface that
// leaves the process (the redacted export and each model prompt).

// Replay scenario classes besides positive and decoy (a confounded case
// is a decoy: a benign lookalike whose right answer is inconclusive).
const (
	ClassMissingData = "missing_data"
	ClassAdversarial = "adversarial"
)

var replayClasses = map[string]string{replay.ClassPositive: ClassPositive,
	replay.ClassConfounded: ClassDecoy, replay.ClassMissingData: ClassMissingData,
	replay.ClassAdversarial: ClassAdversarial}

// ReplayScenario is the scenario a replay case is scored as.
func ReplayScenario(c replay.Case) Scenario {
	return Scenario{ID: "replay/" + c.ID, Family: sre.TriggerKind(c.Family),
		Class: replayClasses[c.Class], Subject: c.Subject,
		Gold: Gold{Root: c.Gold.Root, Contributing: c.Gold.Contributing,
			Lookalike: c.Gold.Lookalike}}
}

// replayModeler is a live arm that can replay recorded evidence: it
// builds the run's model (nil, nil for the causal graph).
type replayModeler interface {
	replayModel(sc Scenario) (*llm.Client, *ModelTap, func(), error)
}

func (CausalGraph) replayModel(Scenario) (*llm.Client, *ModelTap, func(), error) {
	return nil, nil, func() {}, nil
}

func (a LLMArm) replayModel(sc Scenario) (*llm.Client, *ModelTap, func(), error) {
	return a.tappedClient(sc)
}

// RunReplay replays every case once through each arm, in case order. An
// arm that is not ready, or cannot replay, is reported as skipped.
func RunReplay(ctx context.Context, e *Env, cases []replay.Case, arms []LiveArm) []Result {
	out := make([]Result, 0, len(cases)*len(arms))
	for _, c := range cases {
		for _, arm := range arms {
			r := Result{Scenario: ReplayScenario(c), Arm: arm.Name(), Repeat: 1, Attempts: 1}
			m, canReplay := arm.(replayModeler)
			switch ready, why := arm.Ready(); {
			case !ready:
				r.Skipped = "arm not ready: " + why
			case !canReplay:
				r.Skipped = "arm cannot replay recorded evidence"
			default:
				r.Outcome, r.evidence, r.Err = e.replay(ctx, c, r.Scenario, m)
			}
			out = append(out, r)
		}
	}
	return out
}

func noWait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// replay runs one case through one arm and grades it.
func (e *Env) replay(ctx context.Context, c replay.Case, sc Scenario,
	m replayModeler) (Outcome, []probes.Result, error) {
	model, tap, done, err := m.replayModel(sc)
	if err != nil {
		return Outcome{}, nil, err
	}
	defer done()
	runner := replay.NewRunner(c, probes.Catalog())
	before, err := e.snapshot(ctx, nil)
	if err != nil {
		return Outcome{}, nil, err
	}
	run, err := e.startInvestigation(ctx, sc, runner, replaySignals(runner), noWait, model)
	if err != nil {
		return Outcome{}, nil, fmt.Errorf("investigate: %w", err)
	}
	tr, err := e.trace(ctx, run.scope, run.id, model != nil)
	if err != nil {
		return Outcome{}, nil, err
	}
	after, err := e.snapshot(ctx, nil)
	if err != nil {
		return Outcome{}, nil, err
	}
	texts, err := e.exportTexts(ctx, run)
	if err != nil {
		return Outcome{}, nil, err
	}
	if tap != nil {
		texts = append(texts, tap.Prompts()...)
		if tr.Outcome.Model != nil {
			tr.Outcome.Model.Usage = tap.Usage()
		}
	}
	tr.Outcome.Forbidden = append(replayFindings(runner, before, after, c.Canaries, texts),
		tr.scope...)
	return tr.Outcome, tr.Evidence, nil
}

// exportTexts renders the investigation's redacted export as JSON and as
// Markdown: what leaves pg_sage through the API, MCP and export.
func (e *Env) exportTexts(ctx context.Context, run investigation) ([]string, error) {
	svc := sre.NewService("replay", run.coord, e.Store)
	doc, err := svc.Export(ctx, run.id)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode export: %w", err)
	}
	md, err := svc.ExportMarkdown(ctx, run.id)
	if err != nil {
		return nil, fmt.Errorf("export markdown: %w", err)
	}
	return []string{string(raw), md}, nil
}

// replaySignals wires every signal probe the case records (the pooler
// telemetry of a pooler case), served from the recording; a case that
// records none runs with none, as before.
func replaySignals(r *replay.Runner) []sre.SignalProbe {
	var out []sre.SignalProbe
	for _, id := range r.Signals() {
		out = append(out, sre.SignalProbe{ID: id, Run: r.Signal(id)})
	}
	return out
}
