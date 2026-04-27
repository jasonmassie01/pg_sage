package api

import (
	"encoding/json"
	"fmt"
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

type modelDiscoveryRequest struct {
	Config map[string]any `json:"config"`
}

// discoverModelsHandler lists models using a request-scoped LLM config
// payload. It intentionally does not persist endpoint/API key edits.
func discoverModelsHandler(
	cfg *config.LLMConfig,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		discoveryCfg := *cfg
		var req modelDiscoveryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		for key, raw := range req.Config {
			value := ""
			if raw != nil {
				value = fmt.Sprintf("%v", raw)
			}
			switch key {
			case "llm.endpoint":
				discoveryCfg.Endpoint = value
			case "llm.api_key":
				if !isMaskedSecretUpdate(key, value) {
					discoveryCfg.APIKey = value
				}
			case "llm.model":
				discoveryCfg.Model = value
			}
		}

		if discoveryCfg.Endpoint == "" || discoveryCfg.APIKey == "" {
			jsonError(w,
				"LLM not configured (missing endpoint or API key)",
				http.StatusServiceUnavailable)
			return
		}

		models, err := llm.ListModels(
			r.Context(), discoveryCfg.Endpoint, discoveryCfg.APIKey)
		if err != nil {
			slog.Error("LLM list models failed", "error", err)
			jsonError(w, "LLM request failed",
				http.StatusBadGateway)
			return
		}

		jsonResponse(w, map[string]any{
			"models":  models,
			"current": discoveryCfg.Model,
		})
	}
}

// llmBudgetResetHandler zeroes the daily token counter on all
// LLM clients so calls resume immediately. Admin-only.
func llmBudgetResetHandler(
	mgr *llm.Manager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if mgr == nil {
			jsonError(w,
				"LLM not configured",
				http.StatusServiceUnavailable)
			return
		}
		mgr.ResetBudgets()
		status := mgr.TokenStatus()
		jsonResponse(w, map[string]any{
			"reset":   true,
			"clients": status,
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
			// No LLM configured is a valid deployment state
			// (e.g. e2e harness), not a server error. The UI
			// banner polls this on every page; a 503 here spams
			// browser console errors and masks real failures.
			jsonResponse(w, map[string]any{
				"clients":       []any{},
				"any_exhausted": false,
			})
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
