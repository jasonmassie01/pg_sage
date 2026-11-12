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
			row := endpointCredentialStatus(ep)
			addScaleToZeroStatus(row, ep.Endpoint, rt.LakebaseEndpointInfo)
			endpoints = append(endpoints, row)
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

// addScaleToZeroStatus adds what pg_sage knows about the endpoint's
// scale-to-zero. Unobserved fields are null rather than false or 0: pg_sage
// cannot say whether it keeps a compute active it has not read.
func addScaleToZeroStatus(
	row map[string]any, endpoint string,
	lookup func(string) (databricksauth.EndpointInfo, bool),
) {
	row["suspend_timeout_seconds"] = nil
	row["compute_state"] = nil
	row["compute_observed_at"] = nil
	row["keeps_compute_active"] = nil
	row["scale_to_zero_blockers"] = []string{}
	if lookup == nil {
		return
	}
	info, ok := lookup(endpoint)
	if !ok {
		return
	}
	if info.SuspendTimeout > 0 {
		row["suspend_timeout_seconds"] = int64(info.SuspendTimeout / time.Second)
	}
	if info.State != "" {
		row["compute_state"] = info.State
	}
	row["compute_observed_at"] = optionalTime(info.ObservedAt)
	// Blockers come from pool settings noted earlier; with the latest
	// observation showing scale-to-zero off there is nothing for them to block.
	active := info.SuspendTimeout > 0 && len(info.Blockers) > 0
	row["keeps_compute_active"] = active
	if active {
		row["scale_to_zero_blockers"] = info.Blockers
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
