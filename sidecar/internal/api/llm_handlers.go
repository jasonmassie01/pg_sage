package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/llm"
)

// listModelsHandler returns available LLM models from the
// configured provider. Results are cached in the llm package.
func listModelsHandler(
	cfg *config.LLMConfig,
	controllers ...*config.ConfigController,
) http.HandlerFunc {
	return listModelsWithDiscovery(cfg, controllers, databricksauth.ListModelServices)
}

type gatewayModelLister func(context.Context, string, string) ([]databricksauth.GatewayModel, error)

func listModelsWithDiscovery(cfg *config.LLMConfig, controllers []*config.ConfigController, discover gatewayModelLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := activeLLMConfig(cfg, controllers)
		if err := cfg.ValidateDatabricksApp(); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if cfg.Endpoint == "" || (cfg.APIKey == "" && cfg.Auth != "databricks") {
			jsonError(w,
				"LLM not configured (missing endpoint or API key)",
				http.StatusServiceUnavailable)
			return
		}

		models, err := modelsForConfig(r.Context(), cfg, http.DefaultClient, discover)
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

// Model discovery follows committed runtime config, never a startup pointer or
// a desired revision whose reconfiguration has not completed.
func activeLLMConfig(
	fallback *config.LLMConfig, controllers []*config.ConfigController,
) config.LLMConfig {
	if controller := firstConfigController(controllers); controller != nil {
		return controller.Active().Config.LLM
	}
	if fallback != nil {
		return *fallback
	}
	return config.LLMConfig{}
}

type modelDiscoveryRequest struct {
	Config map[string]any `json:"config"`
}

// discoverModelsHandler lists models using a request-scoped LLM config
// payload. It intentionally does not persist endpoint/API key edits.
func discoverModelsHandler(
	cfg *config.LLMConfig,
	controllers ...*config.ConfigController,
) http.HandlerFunc {
	return discoverModelsWithDiscovery(cfg, controllers, databricksauth.ListModelServices)
}

func discoverModelsWithDiscovery(cfg *config.LLMConfig, controllers []*config.ConfigController, discover gatewayModelLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		discoveryCfg := activeLLMConfig(cfg, controllers)
		configuredEndpoint := discoveryCfg.Endpoint
		configuredAuth := discoveryCfg.Auth
		endpointSupplied := false
		apiKeySupplied := false
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
			case "llm.auth":
				if value != "" && value != "api_key" && value != "databricks" {
					jsonError(w, "invalid LLM authentication mode", http.StatusBadRequest)
					return
				}
				discoveryCfg.Auth = value
			case "llm.endpoint":
				discoveryCfg.Endpoint = value
				endpointSupplied = true
			case "llm.api_key":
				if !isMaskedSecretUpdate(key, value) {
					discoveryCfg.APIKey = value
					apiKeySupplied = value != ""
				}
			case "llm.model":
				discoveryCfg.Model = value
			}
		}
		if err := discoveryCfg.ValidateDatabricksApp(); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if endpointSupplied && discoveryCfg.Auth == "databricks" {
			// Native discovery calls the SDK workspace catalog. Never allow a
			// request-scoped endpoint to redirect workspace credentials.
			// Initial setup is validated against the SDK's workspace by the
			// native lister. Existing native config also pins its API origin.
			if configuredAuth == "databricks" && configuredEndpoint != "" &&
				!sameLLMOrigin(discoveryCfg.Endpoint, configuredEndpoint) {
				jsonError(w, "Databricks endpoint override must use the configured HTTPS workspace origin", http.StatusBadRequest)
				return
			}
		} else if endpointSupplied {
			if err := validateLLMDiscoveryEndpoint(
				r.Context(), discoveryCfg.Endpoint,
			); err != nil {
				jsonError(w, "invalid LLM endpoint", http.StatusBadRequest)
				return
			}
			if !sameLLMEndpoint(discoveryCfg.Endpoint, configuredEndpoint) &&
				!apiKeySupplied {
				jsonError(w,
					"API key required for endpoint override",
					http.StatusBadRequest)
				return
			}
		}

		if discoveryCfg.Endpoint == "" || (discoveryCfg.APIKey == "" && discoveryCfg.Auth != "databricks") {
			jsonError(w,
				"LLM not configured (missing endpoint or API key)",
				http.StatusServiceUnavailable)
			return
		}

		httpClient := http.DefaultClient
		if endpointSupplied {
			httpClient = safeLLMDiscoveryHTTPClient()
		}
		models, err := modelsForConfig(r.Context(), discoveryCfg, httpClient, discover)
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

func modelsForConfig(ctx context.Context, cfg config.LLMConfig, client *http.Client, discover gatewayModelLister) ([]llm.ModelInfo, error) {
	if err := cfg.ValidateDatabricksApp(); err != nil {
		return nil, err
	}
	if cfg.Auth != "databricks" {
		return llm.ListModelsWithClient(ctx, cfg.Endpoint, cfg.APIKey, client)
	}
	schema := "system.ai"
	parts := strings.Split(strings.TrimPrefix(cfg.Model, "model-services/"), ".")
	if len(parts) == 3 && parts[0] != "" && parts[1] != "" {
		schema = parts[0] + "." + parts[1]
	}
	services, err := discover(ctx, cfg.Endpoint, schema)
	if err != nil {
		return nil, err
	}
	models := make([]llm.ModelInfo, 0, len(services))
	for _, service := range services {
		models = append(models, llm.ModelInfo{ID: service.ID, Name: service.ID, Description: service.Description})
	}
	return models, nil
}

// LLMBudgetRegistry reports and resets the token budget of every LLM
// client the process can spend through. *llm.Manager satisfies it for the
// shared general/optimizer pair; the sidecar passes its client registry so
// optimizer, per-database and fleet-budget spend are visible (G3-B14).
type LLMBudgetRegistry interface {
	TokenStatus() map[string]llm.ClientStatus
	ResetBudgets()
}

// llmBudgetSource prefers the process registry and falls back to the
// shared manager; nil means no LLM is configured.
func llmBudgetSource(
	registry LLMBudgetRegistry, mgr *llm.Manager,
) LLMBudgetRegistry {
	if registry != nil {
		return registry
	}
	if mgr != nil {
		return mgr
	}
	return nil
}

func registerLLMBudgetRoutes(mux *http.ServeMux, budgets LLMBudgetRegistry) {
	mux.HandleFunc("GET /api/v1/llm/status", llmStatusHandler(budgets))
	mux.Handle("POST /api/v1/llm/budget/reset", RequireRole("admin")(
		http.HandlerFunc(llmBudgetResetHandler(budgets))))
}

// llmBudgetResetHandler zeroes the daily token counter on all
// LLM clients so calls resume immediately. Admin-only.
func llmBudgetResetHandler(
	mgr LLMBudgetRegistry,
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
// for every registered client.
func llmStatusHandler(
	mgr LLMBudgetRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if mgr == nil {
			// No LLM configured is a valid deployment state
			// (e.g. e2e harness), not a server error. The UI
			// banner polls this on every page; a 503 here spams
			// browser console errors and masks real failures.
			jsonResponse(w, map[string]any{
				"clients":         []any{},
				"any_exhausted":   false,
				"databricks_auth": databricksauth.CurrentAuthMetadata(),
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
			"clients":         status,
			"any_exhausted":   anyExhausted,
			"databricks_auth": databricksauth.CurrentAuthMetadata(),
		})
	}
}
