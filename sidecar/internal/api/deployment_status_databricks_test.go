package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

func deploymentStatus(t *testing.T, cfg *config.Config, rt RuntimeDeps) (map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	deploymentStatusHandler(cfg, rt, nil).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/deployment/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result, w.Body.String()
}

func databricksBlock(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	block, ok := result["databricks"].(map[string]any)
	if !ok {
		t.Fatalf("databricks block missing or not an object: %v", result["databricks"])
	}
	return block
}

// Happy path: a minted endpoint reports its expiry and failure class, and
// neither the token nor the mint error text reaches the response.
func TestDeploymentStatusReportsLakebaseCredentialExpiryWithoutSecrets(t *testing.T) {
	expires := time.Date(2026, 11, 11, 13, 0, 0, 0, time.UTC)
	minted := expires.Add(-time.Hour)
	rt := RuntimeDeps{LakebaseCredentials: func() []databricksauth.EndpointCredential {
		return []databricksauth.EndpointCredential{{
			Endpoint: "projects/p/branches/b/endpoints/primary",
			State: databricksauth.CredentialState{LastMintAt: minted, ExpiresAt: expires,
				ConsecutiveFailures: 2, LastErrorClass: "throttled",
				LastError: "mint failed for token secret-fake-token-123"},
		}}
	}}
	result, body := deploymentStatus(t, config.DefaultConfig(), rt)
	if strings.Contains(body, "secret-fake-token") || strings.Contains(body, "mint failed") {
		t.Fatalf("credential secret or error text escaped: %s", body)
	}
	block := databricksBlock(t, result)
	endpoints, ok := block["endpoints"].([]any)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("endpoints = %v; want one", block["endpoints"])
	}
	ep := endpoints[0].(map[string]any)
	if ep["expires_at"] != expires.Format(time.RFC3339) ||
		ep["last_minted_at"] != minted.Format(time.RFC3339) {
		t.Fatalf("endpoint times = %v; want minted %v expires %v", ep, minted, expires)
	}
	if ep["endpoint"] != "projects/p/branches/b/endpoints/primary" ||
		ep["consecutive_mint_failures"] != float64(2) || ep["last_error_class"] != "throttled" {
		t.Fatalf("endpoint row = %v", ep)
	}
	if block["active"] != true || result["runtime_liveness"] != "partial" {
		t.Fatalf("active=%v liveness=%v; want true and partial", block["active"],
			result["runtime_liveness"])
	}
}

// Nil/empty: without Lakebase the block is present with an empty endpoint
// list (not null), an empty telemetry map, and active false.
func TestDeploymentStatusDatabricksBlockIsPresentButEmptyOutsideDatabricks(t *testing.T) {
	for name, rt := range map[string]RuntimeDeps{
		"no source": {},
		"nil list": {LakebaseCredentials: func() []databricksauth.EndpointCredential {
			return nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			result, body := deploymentStatus(t, config.DefaultConfig(), rt)
			if !strings.Contains(body, `"endpoints":[]`) {
				t.Fatalf("endpoints not an empty array: %s", body)
			}
			block := databricksBlock(t, result)
			if block["active"] != false {
				t.Fatalf("active = %v outside Databricks", block["active"])
			}
			if tel, ok := block["telemetry"].(map[string]any); !ok || len(tel) != 0 {
				t.Fatalf("telemetry = %v; want an empty object", block["telemetry"])
			}
			gateway, ok := block["gateway"].(map[string]any)
			if !ok {
				t.Fatalf("gateway = %v; want an object", block["gateway"])
			}
			for _, key := range []string{"authentication_observed", "last_auth_success_at",
				"last_auth_failure_at", "last_failure_class"} {
				if _, present := gateway[key]; !present {
					t.Fatalf("gateway lacks %s: %v", key, gateway)
				}
			}
			if gateway["authentication_observed"] != false {
				t.Fatalf("gateway observed without any request: %v", gateway)
			}
		})
	}
}

// State: each Databricks signal alone marks the block active.
func TestDeploymentStatusDatabricksActiveSignals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*config.Config)
		rt     RuntimeDeps
	}{
		"app auth":    {rt: RuntimeDeps{DatabricksAppAuth: true}},
		"gateway llm": {mutate: func(c *config.Config) { c.LLM.Auth = "databricks" }},
		"lakebase observer": {mutate: func(c *config.Config) {
			c.LakebaseObservability.Enabled = true
		}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			result, _ := deploymentStatus(t, cfg, tc.rt)
			if databricksBlock(t, result)["active"] != true {
				t.Fatalf("%s did not mark the databricks block active", name)
			}
		})
	}
}

// The telemetry block is the per-binding projection TelemetryStatus returns.
func TestDeploymentStatusEmbedsTelemetryPerBinding(t *testing.T) {
	rt := RuntimeDeps{TelemetryStatus: func() any {
		return map[string]any{"orders": map[string]any{"state": "polling",
			"streams": []any{map[string]any{"stream": "plan_history", "statements": 12}}}}
	}}
	result, _ := deploymentStatus(t, config.DefaultConfig(), rt)
	tel, ok := databricksBlock(t, result)["telemetry"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry = %v", databricksBlock(t, result)["telemetry"])
	}
	orders, ok := tel["orders"].(map[string]any)
	if !ok || orders["state"] != "polling" {
		t.Fatalf("telemetry binding = %v; want orders polling", tel)
	}
	streams := orders["streams"].([]any)
	if streams[0].(map[string]any)["statements"] != float64(12) {
		t.Fatalf("stream counters lost: %v", streams)
	}
}

// Boundary: an endpoint that has never minted reports null times, not the
// zero time.
func TestDeploymentStatusEndpointWithoutMintHasNullTimes(t *testing.T) {
	rt := RuntimeDeps{LakebaseCredentials: func() []databricksauth.EndpointCredential {
		return []databricksauth.EndpointCredential{{Endpoint: "projects/p/branches/b/endpoints/x",
			State: databricksauth.CredentialState{ConsecutiveFailures: 1,
				LastErrorClass: "permission_denied"}}}
	}}
	result, body := deploymentStatus(t, config.DefaultConfig(), rt)
	if strings.Contains(body, "0001-01-01") {
		t.Fatalf("zero time leaked: %s", body)
	}
	ep := databricksBlock(t, result)["endpoints"].([]any)[0].(map[string]any)
	if ep["expires_at"] != nil || ep["last_minted_at"] != nil {
		t.Fatalf("never-minted endpoint times = %v; want null", ep)
	}
}
