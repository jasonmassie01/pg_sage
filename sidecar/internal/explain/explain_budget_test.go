package explain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-05: explain had no bound on the plan text it sends. The plan now
// trims to min(maxExplainPlanBytes, llm.context_budget_tokens × 4), after
// redaction, on a rune boundary, and says it was cut.

func bigPlan(bytes int) json.RawMessage {
	node := `{"Node Type":"Seq Scan","Relation Name":"t_é","Filter":"(a = 'secret')"},`
	n := bytes/len(node) + 1
	return json.RawMessage(`[{"Plan":{"Plans":[` +
		strings.TrimSuffix(strings.Repeat(node, n), ",") + `]}}]`)
}

// planBlock returns the body of the plan's data block.
func planBlock(t *testing.T, user string) string {
	t.Helper()
	i := strings.Index(user, `<data label="plan">`+"\n")
	if i < 0 {
		t.Fatalf("no plan block in %.200q", user)
	}
	rest := user[i+len(`<data label="plan">`)+1:]
	end := strings.LastIndex(rest, "\n</data>")
	if end < 0 {
		t.Fatalf("plan block not closed: %.200q", rest[len(rest)-200:])
	}
	return rest[:end]
}

func TestExplainPlanLimit(t *testing.T) {
	for _, tc := range []struct{ budget, want int }{
		{0, maxExplainPlanBytes},
		{-8, maxExplainPlanBytes},
		{8192, 8192},
		{maxExplainPlanBytes + 1, maxExplainPlanBytes},
	} {
		if got := explainPlanLimit(tc.budget); got != tc.want {
			t.Errorf("explainPlanLimit(%d) = %d, want %d", tc.budget, got, tc.want)
		}
	}
}

func TestExplainUserPrompt_ClipsPlanToBudget(t *testing.T) {
	result := &ExplainResult{Query: "SELECT 1", PlanJSON: bigPlan(200 * 1024)}
	for _, budget := range []int{8192, 32768, 0} {
		limit := explainPlanLimit(budget)
		user := explainUserPrompt(result, budget)
		body := planBlock(t, user)
		if !utf8.ValidString(body) {
			t.Fatalf("budget %d: clipped plan is not valid UTF-8", budget)
		}
		if len(body) > limit+len(planClipNote(limit, 1<<30)) {
			t.Fatalf("budget %d: plan = %d bytes, limit %d", budget, len(body), limit)
		}
		if len(body) < limit/2 {
			t.Fatalf("budget %d: plan = %d bytes, cut far below %d", budget,
				len(body), limit)
		}
		if !strings.Contains(body, "plan truncated") {
			t.Fatalf("budget %d: a clipped plan does not say so", budget)
		}
		if strings.Contains(user, "secret") {
			t.Fatalf("budget %d: clipping leaked a literal", budget)
		}
		if strings.Count(user, "</data>") != 2 {
			t.Fatalf("budget %d: want query and plan blocks closed", budget)
		}
	}
}

func TestExplainUserPrompt_SmallPlanUnchanged(t *testing.T) {
	plan := json.RawMessage(`[{"Plan":{"Node Type":"Result"}}]`)
	result := &ExplainResult{Query: "SELECT 1", PlanJSON: plan}
	want := llm.SanitizePromptSQL("plan", string(plan))
	if got := explainUserPrompt(result, 8192); !strings.Contains(got, want) {
		t.Fatalf("small plan changed: %q", got)
	}
	if got := explainUserPrompt(&ExplainResult{Query: "SELECT 1"}, 8192); strings.Contains(
		got, "truncated") {
		t.Fatalf("empty plan marked truncated: %q", got)
	}
}

func TestEnhanceWithLLM_PlanBoundedByClientBudget(t *testing.T) {
	var got capturedChat
	srv := capturingLLMServer(t, &got)
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1000000,
		ContextBudgetTokens: 2048}, noopLogFn)
	ex := NewWithLLM(nil, &config.ExplainConfig{}, client, noopLogFn)
	result := &ExplainResult{Query: "SELECT 1", PlanJSON: bigPlan(200 * 1024)}
	if degraded := ex.enhanceWithLLM(context.Background(), result); degraded {
		t.Fatal("enhancement degraded")
	}
	body := planBlock(t, got.user)
	if len(body) > 8192+len(planClipNote(8192, 1<<30)) || len(body) < 4096 {
		t.Fatalf("wire plan = %d bytes, want about the 8192-byte budget", len(body))
	}
}

func TestClipPlan_CutsOnARuneBoundary(t *testing.T) {
	plan := strings.Repeat("é", 100) // 200 bytes, every rune 2 bytes
	for _, limit := range []int{1, 51, 199} {
		got := clipPlan(plan, limit)
		kept := strings.TrimSuffix(got, planClipNote(limit-1, len(plan)))
		if kept == got {
			t.Fatalf("limit %d: %q does not end in a note for %d kept bytes",
				limit, got, limit-1)
		}
		if !utf8.ValidString(kept) || len(kept) != limit-1 {
			t.Fatalf("limit %d: kept %d bytes (valid=%v), want %d on a rune boundary",
				limit, len(kept), utf8.ValidString(kept), limit-1)
		}
	}
	if got := clipPlan(plan, 200); got != plan {
		t.Fatalf("a plan at the limit was changed: %q", got)
	}
}

// The plan is redacted before it is clipped: a cut through a literal must
// never leave part of the literal in the prompt.
func TestExplainUserPrompt_CutThroughALiteralLeaksNothing(t *testing.T) {
	plan := `[{"Plan":{"Node Type":"Seq Scan",` +
		`"Filter":"(email = 'alice.secret@example.com'::text)"}}]`
	cutAt := strings.Index(plan, "secret")
	result := &ExplainResult{Query: "SELECT 1", PlanJSON: json.RawMessage(plan)}
	for _, budget := range []int{cutAt - 6, cutAt, cutAt + 3, cutAt + 12} {
		user := explainUserPrompt(result, budget)
		for _, part := range []string{"alice", "secret", "example.com"} {
			if strings.Contains(user, part) {
				t.Fatalf("budget %d: prompt leaks %q: %q", budget, part, user)
			}
		}
	}
}

// A cut through a long number must not leave its leading digits: they
// are redacted (6+ digits in a comparison) before the plan is clipped.
func TestExplainUserPrompt_CutThroughANumberLeaksNoDigits(t *testing.T) {
	plan := `[{"Plan":{"Node Type":"Seq Scan","Filter":"(pan = 4111111111111111)"}}]`
	cutAt := strings.Index(plan, "4111")
	result := &ExplainResult{Query: "SELECT 1", PlanJSON: json.RawMessage(plan)}
	for _, budget := range []int{cutAt + 2, cutAt + 4, cutAt + 5, cutAt + 9} {
		if user := explainUserPrompt(result, budget); strings.Contains(user, "41") {
			t.Fatalf("budget %d: prompt keeps digits of the number: %q", budget, user)
		}
	}
}
