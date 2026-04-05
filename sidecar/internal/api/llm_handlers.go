package api

import (
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// listModelsHandler returns available LLM models from the
// configured provider. Results are cached in the llm package.
func listModelsHandler(
	cfg *config.LLMConfig,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Endpoint == "" || cfg.APIKey == "" {
			jsonError(w,
				"LLM not configured (missing endpoint or API key)",
				http.StatusServiceUnavailable)
			return
		}

		models, err := llm.ListModels(
			r.Context(), cfg.Endpoint, cfg.APIKey)
		if err != nil {
			slog.Error("LLM list models failed", "error", err)
			jsonError(w, "LLM request failed",
				http.StatusBadGateway)
			return
		}

		jsonResponse(w, map[string]any{
			"models":  models,
			"current": cfg.Model,
		})
	}
}

// llmStatusHandler returns the current LLM token budget status
// for all configured clients (general and optimizer).
func llmStatusHandler(
	mgr *llm.Manager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if mgr == nil {
			jsonError(w,
				"LLM not configured",
				http.StatusServiceUnavailable)
			return
		}
		status := mgr.TokenStatus()

		// Compute an aggregate exhaustion flag so the UI can
		// show a single banner when any client is out of budget.
		anyExhausted := false
		for _, s := range status {
			if s.Exhausted {
				anyExhausted = true
				break
			}
		}

		jsonResponse(w, map[string]any{
			"clients":       status,
			"any_exhausted": anyExhausted,
		})
	}
}
