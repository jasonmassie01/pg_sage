package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func TestWave3LiveBodyClaimsCannotAuthorizeProviderCall(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	t.Cleanup(pool.Close)
	id := "wave3_body_authority"
	cleanupAgentDBTestRows(t, ctx, pool, id)
	previous, previousErr := st.ProviderConfig(ctx, agentdb.ProviderAWSRDS)
	if previousErr != nil && !errors.Is(previousErr, agentdb.ErrNotFound) {
		t.Fatalf("read existing provider config: %v", previousErr)
	}
	t.Cleanup(func() {
		if previousErr == nil {
			_, _ = st.UpsertProviderConfig(ctx, agentdb.ProviderConfigRequest{
				Provider: previous.Provider, Enabled: previous.Enabled,
				Settings: previous.Settings,
			})
			return
		}
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.agent_db_provider_configs WHERE provider=$1",
			agentdb.ProviderAWSRDS,
		)
	})
	seedEnabledProviderConfig(t, ctx, st, agentdb.ProviderAWSRDS)
	if _, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID: id, TenantID: "wave3", AgentID: "body-claims",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	runner := &wave3APICountingRunner{}
	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(runner)
	body := `{
		"mode":"live","approved":true,"estimated_cost_usd":0.01,
		"estimated_cost_doubled":false,"cost_estimate_id":"made-up",
		"actor_id":"admin","admin_override_reason":"from body"
	}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/"+id+"/provision/execute",
		strings.NewReader(body),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouterWithRegistry(st, registry, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("live body claims status = %d, want 400; body=%s",
			rr.Code, rr.Body.String())
	}
	if got := runner.createCalls.Load(); got != 0 {
		t.Fatalf("provider create calls = %d, want 0", got)
	}
	dep, err := st.Get(ctx, id)
	if err != nil || dep.ProvisioningStatus != "preflight_passed" {
		t.Fatalf("deployment changed after rejected body claims: dep=%+v err=%v", dep, err)
	}
}

func TestWave3ProviderPolicyMutationIsAdminOnly(t *testing.T) {
	mux := http.NewServeMux()
	registerAgentDBRoutes(mux, agentdb.NewStore(nil))
	body := `{"enabled":false,"settings":{"allowed_regions":[]}}`

	operatorRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/provider-configs/aws_rds",
		strings.NewReader(body),
	)
	operatorResponse := httptest.NewRecorder()
	mux.ServeHTTP(operatorResponse, withUser(operatorRequest, testOperatorUser()))
	if operatorResponse.Code != http.StatusForbidden {
		t.Fatalf("operator mutation status = %d, want 403; body=%s",
			operatorResponse.Code, operatorResponse.Body.String())
	}

	adminRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/provider-configs/aws_rds",
		strings.NewReader(body),
	)
	adminResponse := httptest.NewRecorder()
	mux.ServeHTTP(adminResponse, withUser(adminRequest, testAdminUser()))
	if adminResponse.Code == http.StatusForbidden ||
		adminResponse.Code == http.StatusUnauthorized {
		t.Fatalf("admin was blocked by policy mutation role gate: %d",
			adminResponse.Code)
	}
}

func TestWave3ProviderPolicyReadRemainsAvailableToOperators(t *testing.T) {
	mux := http.NewServeMux()
	registerAgentDBRoutes(mux, agentdb.NewStore(nil))
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/provider-configs",
		nil,
	)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, withUser(req, testOperatorUser()))
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("operator provider-policy read was role-blocked: %d", rr.Code)
	}
}

type wave3APICountingRunner struct {
	createCalls atomic.Int64
}

func (*wave3APICountingRunner) Name() string { return "wave3_api_rds" }

func (*wave3APICountingRunner) Provider() string { return agentdb.ProviderAWSRDS }

func (*wave3APICountingRunner) Preflight(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "preflight_passed"}
}

func (r *wave3APICountingRunner) Create(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	r.createCalls.Add(1)
	return agentdb.ProvisionResult{Status: "available"}
}

func (*wave3APICountingRunner) Status(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "available"}
}

func (*wave3APICountingRunner) Destroy(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "destroying"}
}

func (*wave3APICountingRunner) BackupCheck(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "verified"}
}
