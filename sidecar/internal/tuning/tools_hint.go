package tuning

import (
	"context"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// HintExplainer is a HintSink that also runs the hinted EXPLAIN comparison
// (the tuner, tuning-05); the hinted_explain tool is offered only with it.
type HintExplainer interface {
	HintComparisonEnabled() bool
	HintedExplain(ctx context.Context, p tuner.HintProposal) (tuner.HintComparison, error)
}

// hintExplainer is the agent's hinted EXPLAIN, when the hint sink has one
// and it is configured.
func (a *Agent) hintExplainer() (HintExplainer, bool) {
	he, ok := a.deps.Hints.(HintExplainer)
	if !ok || !he.HintComparisonEnabled() {
		return nil, false
	}
	return he, true
}

// hintedExplain: a case statement planned plain and with a hint, at most
// maxHintExplainPerCase times per case. Cited as hint_explain:<qid>:<hash>.
func (tb *toolbox) hintedExplain(ctx context.Context, args toolArgs) toolResult {
	he, ok := tb.a.hintExplainer()
	if !ok {
		return refused("hinted EXPLAIN is not available")
	}
	qid := int64(args.QueryID)
	idx := slices.IndexFunc(tb.c.Statements, func(s CaseStatement) bool {
		return s.QueryID == qid
	})
	if qid == 0 || idx < 0 {
		return refused("queryid %d is not a statement of case %s", qid, tb.c.ID)
	}
	if tb.hinted >= maxHintExplainPerCase {
		return refused("at most %d hinted EXPLAINs per case", maxHintExplainPerCase)
	}
	tb.hinted++
	cmp, err := he.HintedExplain(ctx, tuner.HintProposal{QueryID: qid,
		Query: tb.c.Statements[idx].Text, Hint: args.Hint})
	if err != nil {
		return refused("hinted EXPLAIN refused: %v", err)
	}
	fields := map[string]any{"queryid": fmt.Sprint(qid), "verdict": string(cmp.Verdict),
		"reason": llm.SanitizeForLLM(cmp.Reason), "generic_plan": cmp.Generic,
		"plain_cost": cmp.PlainCost, "hinted_cost": cmp.HintedCost}
	if len(cmp.PlainPlan) > 0 {
		fields["plain_plan"] = clip(llm.SanitizeForLLM(string(cmp.PlainPlan)), maxPlanChars/2)
		fields["hinted_plan"] = clip(llm.SanitizeForLLM(string(cmp.HintedPlan)),
			maxPlanChars/2)
	}
	return toolResult{ref: fmt.Sprintf("hint_explain:%d:%s", qid, cmp.HintedHash),
		fields: fields, text: fmt.Sprintf("hinted EXPLAIN %s: cost %.2f -> %.2f",
			cmp.Verdict, cmp.PlainCost, cmp.HintedCost)}
}
