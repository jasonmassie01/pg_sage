package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
)

// llm-04: GET /api/v1/llm/usage?days=N reports the attributed LLM usage
// ledger: tokens, calls, failures and the operator-priced cost by day,
// database, client role, purpose and model. Viewer access, like
// /api/v1/llm/status.

const (
	defaultLLMUsageDays = 30
	maxLLMUsageDays     = 400 // the ledger's retention
)

// ErrLLMUsageUnavailable means the ledger is not running: no control
// database.
var ErrLLMUsageUnavailable = errors.New("LLM usage ledger is not running: it needs " +
	"the control database")

// LLMUsageRow is one ledger row. EstCostUSD is null when the model has no
// llm.pricing entry (pg_sage never guesses a price); Source is provider or
// estimate (the provider reported no split).
type LLMUsageRow struct {
	Day              string   `json:"day"`
	DatabaseKey      string   `json:"database_key"`
	Database         string   `json:"database,omitempty"`
	LLMRole          string   `json:"llm_role"`
	Purpose          string   `json:"purpose"`
	Model            string   `json:"model"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	Calls            int64    `json:"calls"`
	Failures         int64    `json:"failures"`
	EstCostUSD       *float64 `json:"est_cost_usd"`
	Source           string   `json:"source"`
}

// LLMUsageReader is a registry that reads the attributed usage ledger.
type LLMUsageReader interface {
	LLMUsage(ctx context.Context, days int) ([]LLMUsageRow, error)
}

func llmUsageHandler(budgets LLMBudgetRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, ok := llmUsageDays(r.URL.Query().Get("days"))
		if !ok {
			jsonError(w, "days must be a whole number between 1 and 400",
				http.StatusBadRequest)
			return
		}
		reader, ok := budgets.(LLMUsageReader)
		if !ok || reader == nil {
			jsonError(w, ErrLLMUsageUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
		rows, err := reader.LLMUsage(r.Context(), days)
		switch {
		case errors.Is(err, ErrLLMUsageUnavailable):
			jsonError(w, ErrLLMUsageUnavailable.Error(), http.StatusServiceUnavailable)
			return
		case err != nil:
			slog.Error("LLM usage read failed", "days", days, "error", err)
			jsonError(w, "LLM usage read failed", http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []LLMUsageRow{}
		}
		jsonResponse(w, map[string]any{"days": days, "rows": rows})
	}
}

// llmUsageDays parses ?days: empty is the default, else 1 to 400.
func llmUsageDays(raw string) (int, bool) {
	if raw == "" {
		return defaultLLMUsageDays, true
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > maxLLMUsageDays {
		return 0, false
	}
	return days, true
}
