package api

import (
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

// databricksStatus is the runtime half of the deployment status: credential
// refresh per Lakebase endpoint, the latest Gateway authentication outcomes
// and the telemetry reader's per-binding projection. Like the rest of the
// status it is an allowlist: it names times, counts and failure classes,
// never tokens, error text or SDK objects. endpoints is [] (never null) and
// telemetry {} when nothing is running, so the dashboard can tell "no
// Lakebase" from "not reported".
func databricksStatus(cfg *config.Config, rt RuntimeDeps) map[string]any {
	endpoints := []map[string]any{}
	if rt.LakebaseCredentials != nil {
		for _, ep := range rt.LakebaseCredentials() {
			endpoints = append(endpoints, endpointCredentialStatus(ep))
		}
	}
	var telemetry any = map[string]any{}
	if rt.TelemetryStatus != nil {
		if projected := rt.TelemetryStatus(); projected != nil {
			telemetry = projected
		}
	}
	meta := databricksauth.CurrentAuthMetadata()
	active := rt.DatabricksAppAuth || len(endpoints) > 0 ||
		cfg.LakebaseObservability.Enabled || cfg.LLM.Auth == "databricks" ||
		cfg.LLM.OptimizerAuth() == "databricks"
	return map[string]any{
		"active":    active,
		"endpoints": endpoints,
		"gateway": map[string]any{
			"authentication_observed": meta.AuthenticationObserved,
			"last_auth_success_at":    meta.LastAuthSuccessAt,
			"last_auth_failure_at":    meta.LastAuthFailureAt,
			"last_failure_class":      meta.LastFailureClass,
		},
		"telemetry": telemetry,
	}
}

// endpointCredentialStatus projects one endpoint's refresh state. LastError
// is deliberately left out: only its class is reported.
func endpointCredentialStatus(ep databricksauth.EndpointCredential) map[string]any {
	return map[string]any{
		"endpoint":                  ep.Endpoint,
		"last_minted_at":            optionalTime(ep.State.LastMintAt),
		"expires_at":                optionalTime(ep.State.ExpiresAt),
		"consecutive_mint_failures": ep.State.ConsecutiveFailures,
		"last_error_class":          ep.State.LastErrorClass,
	}
}

// optionalTime reports a zero time as null rather than year 1.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
