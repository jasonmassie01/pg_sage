package api

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

func registerDeploymentStatusRoutes(mux *http.ServeMux, cfg *config.Config, rt RuntimeDeps, clients LLMBudgetRegistry) {
	mux.Handle("GET /api/v1/deployment/status", RequireRole(auth.RoleAdmin)(deploymentStatusHandler(cfg, rt, clients)))
	mux.Handle("GET /api/v1/lakebase-observability/history", RequireRole(auth.RoleAdmin)(telemetryHistoryHandler(rt)))
	mux.Handle("GET /api/v1/lakebase-observability/status", RequireRole(auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if rt.TelemetryStatus == nil {
			jsonResponse(w, map[string]any{"enabled": false, "state": "not_configured", "mode": "metadata_only"})
			return
		}
		jsonResponse(w, rt.TelemetryStatus())
	})))
}

func telemetryHistoryHandler(rt RuntimeDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for key, values := range q {
			if len(values) != 1 || (key != "database" && key != "stream" && key != "from" && key != "to" && key != "limit") {
				jsonError(w, "invalid historical telemetry query", http.StatusBadRequest)
				return
			}
		}
		database, stream := q.Get("database"), q.Get("stream")
		allowedStream := false
		for _, candidate := range []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs, lakebaseobs.ASH, lakebaseobs.DDL, lakebaseobs.ComputeCounters, lakebaseobs.ComputeGauges, lakebaseobs.DatabaseCounters, lakebaseobs.DatabaseGauges, lakebaseobs.WaitCounters, lakebaseobs.StatementCounters} {
			if stream == string(candidate) {
				allowedStream = true
				break
			}
		}
		from, fe := time.Parse(time.RFC3339Nano, q.Get("from"))
		to, te := time.Parse(time.RFC3339Nano, q.Get("to"))
		limit := 100
		var le error
		if raw, supplied := q["limit"]; supplied {
			limit, le = strconv.Atoi(raw[0])
		}
		if database == "" || database == "all" || validateDatabaseParam(database) != nil || !allowedStream || fe != nil || te != nil || !to.After(from) || to.Sub(from) > lakebaseobs.SourceRetention || le != nil || limit < 1 || limit > 1000 {
			jsonError(w, "invalid historical telemetry scope, stream, time window or limit", http.StatusBadRequest)
			return
		}
		if rt.TelemetryHistory == nil {
			jsonError(w, "historical telemetry unavailable", http.StatusServiceUnavailable)
			return
		}
		result, err := rt.TelemetryHistory(r.Context(), database, stream, from.UTC(), to.UTC(), limit)
		if err != nil {
			jsonError(w, "historical telemetry unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, result)
	})
}

// This is deliberately an allowlist, not Config serialization/redaction. It
// describes global configuration; runtime liveness and per-database overrides
// must be checked separately before asserting that a reader is running/stopped.
func deploymentStatusHandler(fallback *config.Config, rt RuntimeDeps, clients LLMBudgetRegistry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var cfg *config.Config
		var generation uint64
		source := "startup_configuration"
		if rt.ConfigController != nil {
			snapshot := rt.ConfigController.Active()
			cfg, generation, source = snapshot.Config, snapshot.Generation, "controller_active"
		} else if fallback != nil {
			// Embedders without a controller supply an immutable startup
			// baseline; production always reads controller.Active above.
			cfg = config.Clone(fallback)
		}
		if cfg == nil {
			jsonError(w, "effective configuration unavailable", http.StatusServiceUnavailable)
			return
		}
		llm, sre := cfg.LLM, cfg.SRE
		optimizerEndpoint := llm.OptimizerLLM.Endpoint
		if optimizerEndpoint == "" {
			optimizerEndpoint = llm.Endpoint
		}
		mode := "session"
		if rt.DatabricksAppAuth {
			mode = auth.DatabricksAppMode
		}
		result := map[string]any{
			"scope": "global_configuration", "source": source, "generation": generation,
			"runtime_liveness": "not_reported", "per_database_overrides": "not_included",
			"auth_mode": mode, "databricks_auth": databricksauth.CurrentAuthMetadata(),
			"ai": map[string]any{
				"general":     map[string]any{"enabled": llm.Enabled, "auth": llm.Auth, "endpoint_origin": statusOrigin(llm.Endpoint), "model": llm.Model, "api_key_configured": llm.APIKey != ""},
				"optimizer":   map[string]any{"enabled": llm.Enabled && llm.OptimizerLLM.Enabled, "auth": llm.OptimizerAuth(), "endpoint_origin": statusOrigin(optimizerEndpoint), "model_override": llm.OptimizerLLM.Model, "api_key_configured": llm.OptimizerLLM.APIKey != "", "fallback_to_general": llm.OptimizerLLM.FallbackToGeneral},
				"sre":         map[string]any{"enabled": sre.LLM.Enabled, "mode": sre.LLM.Mode, "client": "general"},
				"ask_enabled": cfg.Ask.Enabled,
			},
			"sre": map[string]any{
				"automatic_start": sre.AutomaticStart, "read_policy_scope": sre.ReadPolicy.Scope, "read_policy_allowlist_configured": sre.ReadPolicy.AllowedSources != nil, "read_policy_allowed_sources": append([]string{}, sre.ReadPolicy.AllowedSources...),
				"trigger_interval_seconds": sre.TriggerIntervalSeconds, "pooler_count": len(sre.Poolers),
				"runways":           map[string]any{"enabled": sre.Runways.Enabled, "investigate": sre.Runways.Investigate},
				"slo":               map[string]any{"enabled": sre.SLO.Enabled, "open_investigations": sre.SLO.OpenInvestigations, "database_proxies_enabled": sre.SLO.Proxies.Enabled, "prometheus_configured": sre.SLO.Prometheus.URL != "", "push_secret_configured": sre.SLO.Push.HMACSecret != "", "objective_count": len(sre.SLO.Objectives)},
				"change_events":     map[string]any{"feed_enabled": sre.ChangeEvents.FeedEnabled, "ingest_secret_configured": sre.ChangeEvents.HMACSecret != ""},
				"actions":           map[string]any{"proposals": sre.Actions.Proposals, "request_approval": sre.Actions.RequestApproval},
				"autonomy_enforced": sre.Autonomy.Enforce, "game_days_enabled": sre.Autonomy.GameDays.Enabled,
			},
			"external_sinks":     map[string]any{"alerting_enabled": cfg.Alerting.Enabled, "slack_configured": cfg.Alerting.SlackWebhookURL != "", "pagerduty_configured": cfg.Alerting.PagerDutyRoutingKey != "", "webhook_count": len(cfg.Alerting.Webhooks), "siem_sink_count": len(cfg.Audit.SIEM.Sinks), "specialist_pagerduty_result_configured": cfg.Specialist.PagerDuty.APIURL != "", "specialist_webhook_result_configured": cfg.Specialist.Webhook.ResultURL != ""},
			"readers":            map[string]any{"cloud_telemetry_enabled": cfg.CloudTelemetry.IsEnabled(), "logwatch_enabled": cfg.LogWatch.Enabled, "auto_explain_enabled": cfg.AutoExplain.Enabled, "lakebase_observability_enabled": cfg.LakebaseObservability.Enabled, "lakebase_observability_allow_payload": cfg.LakebaseObservability.AllowPayload, "lakebase_observability_binding_count": len(cfg.LakebaseObservability.Bindings)},
			"optional_protocols": map[string]any{"mcp_configured": cfg.MCP.Enabled, "mcp_transport": cfg.MCP.Transport, "specialist_configured": cfg.Specialist.Enabled, "supported_in_app": false, "refused_in_current_profile": rt.DatabricksAppAuth},
			"trust_level":        cfg.Trust.Level,
		}
		if clients != nil {
			result["registered_ai_clients"] = clients.TokenStatus()
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, result)
	})
}

func statusOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
