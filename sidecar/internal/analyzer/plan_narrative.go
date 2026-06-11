package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// LLMPlanNarrator generates a plain-English "why did the plan change"
// narrative for plan_regression findings (C6). This is an LLM-native
// differentiator: nobody explains plan flips well, and the narrative
// drives the operator to the right fix.
type LLMPlanNarrator struct {
	client *llm.Client
	logFn  func(string, string, ...any)
}

// NewLLMPlanNarrator builds a narrator over an LLM client.
func NewLLMPlanNarrator(
	client *llm.Client, logFn func(string, string, ...any),
) *LLMPlanNarrator {
	return &LLMPlanNarrator{client: client, logFn: logFn}
}

const planNarrativeSystem = `You are a PostgreSQL performance expert. A query's ` +
	`execution plan became more expensive. In 2-4 plain-English sentences, explain ` +
	`the most likely cause(s) of the plan change (e.g. stale statistics, data growth, ` +
	`a dropped or newly-created index, a parameter change, or a planner cost ` +
	`mis-estimate) and the single best next step. Be specific to the evidence given. ` +
	`No markdown. Output only the explanation.`

// Narrate enriches each plan_regression finding with an LLM narrative.
// No-op when the client is nil/disabled.
func (n *LLMPlanNarrator) Narrate(ctx context.Context, findings []Finding) []Finding {
	if n == nil || n.client == nil || !n.client.IsEnabled() {
		return findings
	}
	for i := range findings {
		if findings[i].Category != "plan_regression" {
			continue
		}
		nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		narrative, _, err := n.client.Chat(
			nctx, planNarrativeSystem,
			buildPlanNarrativePrompt(findings[i]), 512)
		cancel()
		if err != nil {
			if n.logFn != nil {
				n.logFn("analyzer", "plan narrative failed: %v", err)
			}
			continue
		}
		// json_mode wraps prose in a JSON object; store plain text (C6).
		narrative = strings.TrimSpace(llm.UnwrapText(narrative))
		if narrative == "" {
			continue
		}
		if findings[i].Detail == nil {
			findings[i].Detail = map[string]any{}
		}
		findings[i].Detail["narrative"] = narrative
		findings[i].Recommendation = narrative
	}
	return findings
}

// buildPlanNarrativePrompt assembles the evidence for one regression.
// Pure and testable.
func buildPlanNarrativePrompt(f Finding) string {
	d := f.Detail
	var b strings.Builder
	fmt.Fprintf(&b, "Query: %s\n", planDetailStr(d, "query"))
	fmt.Fprintf(&b, "Cost increase: %.1fx (from %.0f to %.0f)\n",
		planDetailFloat(d, "cost_ratio"),
		planDetailFloat(d, "previous_cost"),
		planDetailFloat(d, "current_cost"))
	if nc := planDetailStrings(d, "node_changes"); len(nc) > 0 {
		fmt.Fprintf(&b, "Plan node changes: %s\n", strings.Join(nc, "; "))
	}
	if spills, _ := d["new_disk_spills"].(bool); spills {
		b.WriteString("New disk spills appeared in the plan.\n")
	}
	if s := planDetailStr(d, "previous_summary"); s != "" {
		fmt.Fprintf(&b, "Previous plan: %s\n", s)
	}
	if s := planDetailStr(d, "current_summary"); s != "" {
		fmt.Fprintf(&b, "Current plan: %s\n", s)
	}
	return b.String()
}

func planDetailStr(d map[string]any, k string) string {
	if d == nil {
		return ""
	}
	s, _ := d[k].(string)
	return s
}

func planDetailFloat(d map[string]any, k string) float64 {
	if d == nil {
		return 0
	}
	switch v := d[k].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

func planDetailStrings(d map[string]any, k string) []string {
	if d == nil {
		return nil
	}
	switch v := d[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
