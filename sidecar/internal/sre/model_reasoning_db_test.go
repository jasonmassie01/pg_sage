package sre

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Reasoning models in the investigator (Sage SRE M3): Gemini 2.5/3,
// OpenAI o-series and DeepSeek R1 get their model turn with an explicit
// reasoning allowance (per turn: MaxReasoningTokens / turns = 8192) on
// top of the unchanged answer cap (2000 per turn). Other models' requests
// are unchanged.

func namedModel(url, model string, daily int) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "k",
		Model: model, TimeoutSeconds: 5, TokenBudgetDaily: daily},
		func(string, string, ...any) {})
}

func maxTokensOf(t *testing.T, body string) int {
	t.Helper()
	var req struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	return req.MaxTokens
}

type reservationRow struct {
	reasoning, reasoningUsed, output int64
	state                            string
}

func reservations(t *testing.T, pool *pgxpool.Pool, inv Investigation) []reservationRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT reasoning_reserved,
		    COALESCE(reasoning_used, 0), output_reserved, state
		FROM sage.sre_budget_reservations
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		ORDER BY created_at`, string(inv.Scope.DeploymentID),
		string(inv.Scope.DatabaseID), string(inv.ID))
	if err != nil {
		t.Fatalf("reservations: %v", err)
	}
	defer rows.Close()
	var out []reservationRow
	for rows.Next() {
		var r reservationRow
		if err := rows.Scan(&r.reasoning, &r.reasoningUsed, &r.output, &r.state); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestReasoningModel_GetsATurnWithExplicitAllowance(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "gemini-3-pro-preview", "o3-mini",
		"deepseek-r1"} {
		t.Run(model, func(t *testing.T) {
			st, pool, ctx := liveStore(t, budgetLimits())
			m := newFakeModel(t, toolReply(validIdleReview(t)))
			c, _ := modelCoordinator(t, ctx, st, idleChainRunner(),
				namedModel(m.srv.URL, model, 1_000_000))
			inv := startAndRun(t, ctx, c, lockTrigger("m3-think-"+model))
			assertIdleRoot(t, st, inv)
			if inv.Summary.ModelRanking == nil || m.calls() != 1 || inv.ModelTurns != 1 {
				t.Fatalf("no model turn: %+v calls=%d", inv.Summary, m.calls())
			}
			if got := maxTokensOf(t, m.body(t, 0)); got != 2000+8192 {
				t.Fatalf("max_tokens = %d, want 2000 answer + 8192 reasoning", got)
			}
			res := reservations(t, pool, inv)
			if len(res) != 1 || res[0].reasoning != 8192 || res[0].output != 2000 ||
				res[0].state != string(ReservationSettled) {
				t.Fatalf("reservations = %+v", res)
			}
		})
	}
}

func TestReasoningModel_NonThinkingRequestUnchanged(t *testing.T) {
	st, pool, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(),
		namedModel(m.srv.URL, "gpt-4o-mini", 1_000_000))
	inv := startAndRun(t, ctx, c, lockTrigger("m3-plain-model"))
	if got := maxTokensOf(t, m.body(t, 0)); got != 2000 || inv.Summary.ModelRanking == nil {
		t.Fatalf("max_tokens = %d, ranking %+v", got, inv.Summary.ModelRanking)
	}
	if res := reservations(t, pool, inv); len(res) != 1 || res[0].reasoning != 0 {
		t.Fatalf("reservations = %+v, want no reasoning hold", res)
	}
}

// The daily budgets still refuse: the client's llm.token_budget_daily
// and the investigator's daily allocation both count the reasoning.
func TestReasoningModel_DailyBudgetsStillRefuse(t *testing.T) {
	small := budgetLimits()
	small.DatabaseDailyTokens, small.DeploymentDailyTokens = 12000, 1_000_000_000
	cases := map[string]struct {
		limits Limits
		daily  int
	}{
		"llm.token_budget_daily":  {budgetLimits(), 5000},
		"investigator allocation": {small, 1_000_000},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st, _, ctx := liveStore(t, tc.limits)
			m := newFakeModel(t, toolReply(validIdleReview(t)))
			c, _ := modelCoordinator(t, ctx, st, idleChainRunner(),
				namedModel(m.srv.URL, "gemini-2.5-flash", tc.daily))
			inv := startAndRun(t, ctx, c, lockTrigger("m3-daily"))
			assertIdleRoot(t, st, inv)
			assertDeterministicOnly(t, inv)
			rej := payloads(t, st, inv, EventModelRejected)
			if m.calls() != 0 || len(rej) != 1 || rej[0]["reason"] != RejectBudget {
				t.Fatalf("calls=%d rejected=%v", m.calls(), rej)
			}
		})
	}
}

// Reasoning over the allowance (the provider reports more) is recorded
// as reported and refuses the repair turn.
func TestReasoningModel_OverrunRecordedAndRefusesSecondTurn(t *testing.T) {
	st, pool, ctx := liveStore(t, budgetLimits())
	bad := wireReview{Ranking: []string{"cosmic_rays"}}.json()
	m := newFakeModel(t, func(w http.ResponseWriter, _ string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant",
				"content": bad}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 3000, "completion_tokens": 500,
				"total_tokens":              18500,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 15000}}})
	}, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(),
		namedModel(m.srv.URL, "gemini-2.5-flash", 1_000_000))
	inv := startAndRun(t, ctx, c, lockTrigger("m3-overrun"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	res := reservations(t, pool, inv)
	if m.calls() != 1 || inv.ModelTurns != 1 || len(res) != 1 ||
		res[0].reasoningUsed != 15000 || res[0].state != string(ReservationSettled) {
		t.Fatalf("calls=%d turns=%d reservations=%+v", m.calls(), inv.ModelTurns, res)
	}
	rej := payloads(t, st, inv, EventModelRejected)
	if len(rej) != 1 || rej[0]["reason"] != RejectBudget {
		t.Fatalf("model_rejected = %v, want the repair turn refused by the budget", rej)
	}
}
