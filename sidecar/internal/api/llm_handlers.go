package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/llm"
)

// listModelsHandler returns available LLM models from the
// configured provider. Results are cached in the llm package.
func listModelsHandler(controller *config.ConfigController) http.HandlerFunc {
	return listModelsWithDiscovery(controller, databricksauth.ListModelServices)
}

// llmModelsHTTPClient lists models at the configured endpoint through the
// egress floor; it is shared so its connections are reused.
var llmModelsHTTPClient = egress.HTTPClient(0)

type gatewayModelLister func(context.Context, string, string) ([]databricksauth.GatewayModel, error)

func listModelsWithDiscovery(
	controller *config.ConfigController, discover gatewayModelLister,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := activeLLMConfig(controller)
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

		models, err := modelsForConfig(r.Context(), cfg, llmModelsHTTPClient, discover)
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
func activeLLMConfig(controller *config.ConfigController) config.LLMConfig {
	return controller.Active().Config.LLM
}

type modelDiscoveryRequest struct {
	Config map[string]any `json:"config"`
}

// discoverModelsHandler lists models using a request-scoped LLM config
// payload. It intentionally does not persist endpoint/API key edits.
func discoverModelsHandler(controller *config.ConfigController) http.HandlerFunc {
	return discoverModelsWithDiscovery(controller, databricksauth.ListModelServices)
}

func discoverModelsWithDiscovery(
	controller *config.ConfigController, discover gatewayModelLister,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		discoveryCfg := activeLLMConfig(controller)
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

		httpClient := llmModelsHTTPClient
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

// LLMGlobalBudgetReporter is a registry that also enforces one
// process-wide ceiling across its clients (llm.global_token_budget_daily).
type LLMGlobalBudgetReporter interface {
	GlobalBudget() llm.BudgetStatus
}

// LLMUsageLedgerReporter is a registry whose daily usage survives restarts
// (fleet-10): it says when today's usage was restored from the control
// database and when it was last flushed there (zero: not yet).
type LLMUsageLedgerReporter interface {
	UsageLedgerStatus() (restoredAt, lastFlushAt time.Time)
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
	route(mux, "GET /api/v1/llm/status", gateViewer, http.HandlerFunc(llmStatusHandler(budgets)))
	route(mux, "POST /api/v1/llm/budget/reset", gateAdmin,
		http.HandlerFunc(llmBudgetResetHandler(budgets)))
	route(mux, "POST /api/v1/llm/test", gateAdmin, http.HandlerFunc(llmTestHandler(budgets)))
	route(mux, "GET /api/v1/llm/usage", gateViewer, http.HandlerFunc(llmUsageHandler(budgets)))
}

// LLMProber is a registry that can test its general client's connection
// (llm.Client.Probe). *llm.Manager and the sidecar registry satisfy it.
type LLMProber interface {
	ProbeLLM(ctx context.Context) llm.ProbeResult
}

// llmTestHandler sends one tiny request through the normal path: charged
// to the budget and admitted as the circuit breaker's half-open probe, so
// a fixed provider closes the breaker now. It answers with the error
// class and latency. Admin-only.
func llmTestHandler(budgets LLMBudgetRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prober, ok := budgets.(LLMProber)
		if !ok || prober == nil {
			jsonError(w, "LLM not configured", http.StatusServiceUnavailable)
			return
		}
		res := prober.ProbeLLM(r.Context())
		slog.Info("LLM connection test", "class", res.Class,
			"latency_ms", res.LatencyMS, "model", res.Model)
		jsonResponse(w, res)
	}
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
		jsonResponse(w, llmStatusPayload(mgr))
	}
}

// llmStatusPayload reports every client and, when the registry enforces
// one, the global ceiling. any_exhausted is the UI's single banner flag.
func llmStatusPayload(mgr LLMBudgetRegistry) map[string]any {
	status := mgr.TokenStatus()
	anyExhausted := false
	for _, s := range status {
		if s.Exhausted {
			anyExhausted = true
			break
		}
	}
	body := map[string]any{
		"clients":         status,
		"databricks_auth": databricksauth.CurrentAuthMetadata(),
	}
	if reporter, ok := mgr.(LLMGlobalBudgetReporter); ok {
		global := reporter.GlobalBudget()
		body["global"] = global
		anyExhausted = anyExhausted || global.Exhausted
	}
	if reporter, ok := mgr.(LLMUsageLedgerReporter); ok {
		restored, flushed := reporter.UsageLedgerStatus()
		body["usage_restored_at"] = ledgerTimestamp(restored)
		body["ledger_last_flush_at"] = ledgerTimestamp(flushed)
	}
	body["any_exhausted"] = anyExhausted
	return body
}

// ledgerTimestamp is RFC 3339 in UTC, or null for a zero time.
func ledgerTimestamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
