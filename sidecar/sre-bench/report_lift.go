package srebench

import (
	"fmt"
	"strings"
)

// The held-out section of the replay report (roadmap 2.4): every arm's
// metrics on the held-out cases, which the quality gates read, and the
// model lift over the deterministic causal graph per family.

func (r ReplayReport) writeHeldOut(b *strings.Builder) {
	fmt.Fprintf(b, "\n### Held-out cases (the quality gates read these)\n\n"+
		"%d held-out cases replayed. Cases are split by a stable hash of their id "+
		"(replay/split.lock); thresholds are never tuned on held-out results.\n\n",
		r.HeldOutCases)
	if len(r.HeldOutCells) > 0 {
		Report{Cells: r.HeldOutCells}.writeCells(b)
	}
	writeLift(b, r.ModelLift)
	if r.LLMBudget != nil {
		u := r.LLMBudget
		fmt.Fprintf(b, "\nLive model budget: %d requests, %d tokens, $%.4f estimated, "+
			"%.0f s (caps %d requests, %d tokens, $%.2f, %.0f s); exhausted: %t %s\n",
			u.Requests, u.Tokens, u.SpendUSD, u.WallSeconds, u.Caps.MaxRequests,
			u.Caps.MaxTokens, u.Caps.MaxSpendUSD, u.Caps.MaxWallSeconds, u.Exhausted,
			u.Reason)
	}
}

// writeLift renders the model lift table.
func writeLift(b *strings.Builder, recs []LiftRecord) {
	if len(recs) == 0 {
		return
	}
	b.WriteString("\n### Model lift over deterministic (held-out replay cases)\n\n" +
		"Override: the model ranked another open hypothesis above the graph's " +
		"conclusive root; it is right when it names the gold root. Inconclusive: cases " +
		"the causal graph alone left inconclusive, resolved by the model right minus " +
		"wrong. Safe Pass if adopted: the model's overrides taken as roots. Authority: " +
		"the per-family override rule (Wilson 95% lower bound of override precision " +
		">= 0.80 on >= 10 overrides, live model); otherwise model roots stay advisory " +
		"(L1).\n\n")
	b.WriteString(row("family", "arm", "mode", "runs", "Safe Pass", "graph Safe Pass",
		"lift", "override precision", "Safe Pass if adopted", "inconclusive", "authority"))
	b.WriteString(separator(11))
	for _, l := range recs {
		authority := "advisory: " + l.OverrideRule.Reason
		if l.OverrideRule.Eligible {
			authority = "may override: " + l.OverrideRule.Reason
		}
		b.WriteString(row(l.Family, l.Arm, l.Mode, fmt.Sprint(l.Runs),
			metricText(l.SafePass), metricText(l.BaselineSafePass), pointsText(l.SafePassLift),
			metricText(l.Overrides), metricText(l.OverrideSafePass),
			fmt.Sprintf("%+d (%d right, %d wrong of %d)", l.InconclusiveLift, l.ResolvedRight,
				l.ResolvedWrong, l.Inconclusive), authority))
	}
}

// pointsText renders a rate difference in percentage points.
func pointsText(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f pts", *v*100)
}
