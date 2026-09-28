package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// G8-B15/SURF-04: readiness is computed from the runtime registry and the
// effective policy, not always "runtime_dependencies_missing".
func TestAgentDBProviderReadinessUsesRuntimeRegistry(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	seedEnabledProviderConfig(t, ctx, st, agentdb.ProviderAWSRDS)
	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(apiFakeProviderRunner{})
	handler := withTestOperator(agentDBSubrouterWithRegistry(
		st, registry, nil, wave34TestLiveAuthority()))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/agent-dbs/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Providers []agentdb.ProviderReadiness `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, row := range body.Providers {
		for _, reason := range row.DisabledReasons {
			if reason.Code == "runtime_dependencies_missing" {
				t.Fatalf("%s readiness ignores runtime: %+v", row.Provider, row)
			}
		}
		if row.Provider == agentdb.ProviderAWSRDS && !row.Found {
			t.Fatalf("configured aws_rds reported not ready: %+v", row)
		}
		if row.Provider == agentdb.ProviderGCPCloudSQL && row.Found {
			t.Fatalf("unconfigured gcp reported ready: %+v", row)
		}
	}
}

// G8-B14: register persists an allow-listed secret_ref.
func TestAgentDBRegisterPersistsSecretRef(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_fix_secret_ref")
	cleanupAgentDBTestRows(t, ctx, pool, "req_fix_secret_ref")
	// D4: cloud registers consume an approved request.
	if _, err := st.CreateRequest(ctx, agentdb.RequestCreate{RequestID: "req_fix_secret_ref",
		TenantID: "t", AgentID: "a", IsolationType: agentdb.LevelInstance,
		Provider: agentdb.ProviderNeon, BudgetUSD: 5}); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-dbs", bytes.NewReader([]byte(`{
		"request_id":"req_fix_secret_ref",
		"deployment_id":"api_fix_secret_ref","tenant_id":"t","agent_id":"a",
		"provider":"neon","provisioning_level":"instance",
		"secret_ref":"env:PG_SAGE_AGENTDB_NEON_DSN","secret_ref_provider":"env"}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	registerAgentDBRoutesWithAuthority(mux, st, nil, nil)
	withTestOperator(mux).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("register status = %d body=%s", rr.Code, rr.Body.String())
	}
	dep, err := st.Get(ctx, "api_fix_secret_ref")
	if err != nil || dep.SecretRef != "env:PG_SAGE_AGENTDB_NEON_DSN" {
		t.Fatalf("secret_ref not persisted: %q err=%v", dep.SecretRef, err)
	}
}
