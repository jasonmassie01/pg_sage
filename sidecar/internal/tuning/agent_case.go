package tuning

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// operatorRejectionWindow is how far back an operator's rejection keeps
// the same SQL from being proposed again.
const operatorRejectionWindow = 30 * 24 * time.Hour

// maxOutcomesPerClass bounds the outcomes the calibration reads per class.
const maxOutcomesPerClass = 500

// cycle is one cycle's shared state.
type cycle struct {
	cur, prev *collector.Snapshot
	w         Workload
	all       []facts.Fact
	confirmed []facts.Fact
	budget    *CycleBudget
	tools     *cycleTools
	v         *validator
	cal       Calibration
	day       dayBudget
	open      []analyzer.Finding // the open findings kept this cycle
}

// askCases asks the model about the cases, within the cycle's case cap,
// its budget and what is left of the day's, and returns the ranked
// findings of what it admitted. Cases left for later are asked first next
// cycle (longest waiting first), so a cap or an exhausted budget never
// starves the same cases.
func (a *Agent) askCases(ctx context.Context, cy *cycle, cases []Case) []analyzer.Finding {
	t := a.settings.Tuning
	tokens, ok := a.openDay(ctx, cy)
	if !ok {
		a.deferAll(cy, cases)
		return nil
	}
	cy.budget = NewCycleBudget(t.MaxRequestsPerCycle, tokens)
	cy.tools = a.cycleTools()
	cy.v = a.newValidator(cy.cur, cy.w, cy.confirmed, a.operatorRejected(ctx))
	cy.v.prepare(ctx, cy.prev, cy.open)
	cy.cal = a.calibration(ctx)
	judged, deferred, asked, stopped := a.askInTurn(ctx, cy, cases)
	a.queue.advance(deferred)
	a.logDeferred(deferred, stopped)
	a.noteCycle(cy, asked, len(deferred))
	return a.recordHints(ctx, a.rank(judged, cy.cal))
}

// askInTurn runs the cases in queue order until the case cap, the budget
// or a spend that cannot be recorded stops the cycle.
func (a *Agent) askInTurn(ctx context.Context, cy *cycle, cases []Case) (
	judged []Judged, deferred []string, asked int, stopped bool) {
	for _, c := range a.queue.order(cases) {
		if stopped || asked >= a.settings.Tuning.MaxCasesPerCycle {
			deferred = append(deferred, c.ID)
			continue
		}
		if a.memory.skip(c, a.now()) {
			a.modelSkips.Add(1)
			a.logFn("DEBUG", "tuning: case %s: recent answers were all wasted and the case "+
				"is unchanged, not asking again", c.ID)
			continue
		}
		res, stop := a.runCase(ctx, cy, c)
		judged = append(judged, res...)
		if err := a.chargeDay(ctx, cy); err != nil {
			a.logFn("WARN", "tuning: the daily budget cannot record this cycle's spend, "+
				"stopping: %v", err)
			stop = true
		}
		if stop {
			stopped, deferred = true, append(deferred, c.ID)
			continue
		}
		asked++
	}
	return judged, deferred, asked, stopped
}

func (a *Agent) operatorRejected(ctx context.Context) map[string]bool {
	rejected, err := a.deps.Store.OperatorRejected(ctx, a.now().Add(-operatorRejectionWindow))
	if err != nil {
		a.logFn("WARN", "tuning: operator rejections unreadable: %v", err)
		return map[string]bool{}
	}
	return rejected
}

// calibration reads the outcome ledger once per cycle.
func (a *Agent) calibration(ctx context.Context) Calibration {
	t := a.settings.Tuning
	classes := []string{verify.ClassIndexCreate, verify.ClassIndexDrop, verify.ClassGUC,
		verify.ClassReloption, verify.ClassStatistics, verify.ClassQueryHint}
	since := a.now().Add(-time.Duration(t.CalibrationWindowDays) * 24 * time.Hour)
	samples, err := a.deps.Store.Outcomes(ctx, classes, since, maxOutcomesPerClass)
	if err != nil {
		a.logFn("WARN", "tuning: outcome ledger unreadable, every proposal is "+
			"uncalibrated this cycle: %v", err)
	}
	return Calibrate(samples, t.CalibrationMinOutcomes)
}

// runCase holds one conversation about c and judges its answer; stop
// reports that the cycle must not ask about more cases (budget spent or
// the provider refusing).
func (a *Agent) runCase(ctx context.Context, cy *cycle, c Case) ([]Judged, bool) {
	pk := a.packetFor(ctx, c, cy.cur, cy.w, cy.confirmed, cy.v.flight.ddl)
	tb := a.newToolbox(c, cy.cur, cy.prev, cy.w, cy.tools)
	conv := Conversation{System: systemPrompt, User: pk.Text + a.prefetch(ctx, tb, c),
		Tools: a.toolSpecs(), Exec: tb.exec, MaxTurns: a.settings.Tuning.MaxTurnsPerCase,
		MaxTokens: a.settings.MaxOutputTokens, Budget: cy.budget}
	tr, err := a.deps.Loop.Run(ctx, a.deps.Model, conv)
	if err != nil && a.deps.Fallback != nil && fallbackWorthy(err) {
		a.logFn("WARN", "tuning: case %s: model failed, trying the fallback: %v", c.ID, err)
		tr, err = a.deps.Loop.Run(ctx, a.deps.Fallback, conv)
	}
	if err != nil {
		return nil, a.caseFailed(c, err)
	}
	answer, err := ParseAnswer(tr.Final)
	if err != nil {
		a.logFn("WARN", "tuning: case %s: unusable answer: %v", c.ID, err)
		a.memory.note(c, caseOutcome{Malformed: true}, a.now())
		return nil, false
	}
	ev := maps.Clone(pk.Evidence)
	maps.Copy(ev, tb.evidence)
	var out []Judged
	wasted := 0
	for _, p := range answer.Proposals {
		j := cy.v.judge(ctx, c, ev, p)
		if j.Verdict == VerdictRejected {
			wasted++
			a.logFn("INFO", "tuning: case %s: %s proposal rejected (%s): %s", c.ID, p.Type,
				j.Reason, j.Detail)
			continue
		}
		out = append(out, j)
	}
	a.memory.note(c, caseOutcome{Proposals: len(answer.Proposals), Wasted: wasted},
		a.now())
	return out, false
}

// fallbackWorthy reports a provider failure another model may not have.
func fallbackWorthy(err error) bool {
	return !errors.Is(err, ErrBudget) && !errors.Is(err, ErrNoAnswer) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// caseFailed logs a failed conversation and says whether the cycle stops.
// An empty or never-ending answer is wasted (case memory counts it); a
// provider failure is neutral.
func (a *Agent) caseFailed(c Case, err error) bool {
	switch {
	case errors.Is(err, ErrBudget):
		a.logFn("INFO", "tuning: case %s: the cycle's model budget is spent: %v", c.ID, err)
		return true
	case errors.Is(err, llm.ErrRateLimited):
		a.logFn("WARN", "tuning: case %s: the provider is rate limiting, no more cases "+
			"this cycle: %v", c.ID, err)
		return true
	case errors.Is(err, context.Canceled):
		return true
	case errors.Is(err, llm.ErrEmptyResponse) || errors.Is(err, ErrNoAnswer):
		a.logFn("WARN", "tuning: case %s: no usable answer: %v", c.ID, err)
		a.memory.note(c, caseOutcome{Malformed: true}, a.now())
		return false
	}
	a.logFn("WARN", "tuning: case %s: model failed: %v", c.ID, err)
	return false
}

// prefetch runs the statement tool for the case's lead statement, so
// every case starts with its statement's counters as evidence (R1).
func (a *Agent) prefetch(ctx context.Context, tb *toolbox, c Case) string {
	if len(c.Statements) == 0 {
		return ""
	}
	args := fmt.Sprintf(`{"queryid":"%d"}`, c.Statements[0].QueryID)
	res := tb.exec(ctx, llm.ToolCall{ID: "prefetch", Name: "statement",
		Arguments: []byte(args)})
	return "\n\nPre-fetched tool result (statement " + fmt.Sprint(c.Statements[0].QueryID) +
		"):\n" + res
}

// isLegacyIndexFinding is index advice of the optimizer before the agent
// (by category or the optimizer's detail markers).
func isLegacyIndexFinding(f analyzer.Finding) bool {
	return f.Category != "query_tuning" && optimizer.IsOptimizerFinding(f.Category, f.Detail)
}
