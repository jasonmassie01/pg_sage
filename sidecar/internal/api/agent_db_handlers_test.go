package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func TestAgentDBSubrouterSetsRequestIDForRequestActions(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_request_route")

	body := []byte(`{
		"request_id":"api_request_route",
		"tenant_id":"tenant_agentdb_api",
		"agent_id":"agent_api",
		"requested_isolation_type":"external"
	}`)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/requests",
		bytes.NewReader(body),
	)
	req.Header.Set("Idempotency-Key", "api-route-idem")
	rr := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create request status = %d body=%s", rr.Code, rr.Body.String())
	}

	getReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/requests/api_request_route",
		nil,
	)
	getRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get request status = %d body=%s", getRR.Code, getRR.Body.String())
	}

	approveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/requests/api_request_route/approve",
		bytes.NewReader([]byte(`{"reason":"reviewed"}`)),
	)
	approveRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(approveRR, approveReq)
	if approveRR.Code != http.StatusOK {
		t.Fatalf("approve request status = %d body=%s", approveRR.Code, approveRR.Body.String())
	}

	var got agentdb.Request
	if err := json.Unmarshal(approveRR.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode approve body: %v", err)
	}
	if got.RequestID != "api_request_route" || got.Status != "approved" {
		t.Fatalf("approved request = %#v", got)
	}
}

func TestAgentDBLifecycleEndpointsExposeCostBackupsAndHints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_deployment_route")

	_, err := st.Register(ctx, agentdb.RegisterRequest{
		DeploymentID:   "api_deployment_route",
		TenantID:       "tenant_agentdb_api",
		AgentID:        "agent_api",
		IsolationType:  "schema",
		BudgetUSD:      2,
		BackupRequired: true,
		Metadata: map[string]any{
			"workload_types": []any{"vector", "jsonb"},
			"extensions":     []any{"pgvector"},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	postJSON := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		rr := httptest.NewRecorder()
		agentDBSubrouter(st).ServeHTTP(rr, req)
		return rr
	}

	if rr := postJSON(
		"/api/v1/agent-dbs/api_deployment_route/cost-samples",
		`{"cost_usd":1.75,"metric":"tokens","value":100,"unit":"count"}`,
	); rr.Code != http.StatusOK {
		t.Fatalf("cost sample status = %d body=%s", rr.Code, rr.Body.String())
	}

	costReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_deployment_route/cost",
		nil,
	)
	costRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(costRR, costReq)
	if costRR.Code != http.StatusOK {
		t.Fatalf("cost status = %d body=%s", costRR.Code, costRR.Body.String())
	}
	var cost map[string]agentdb.CostSummary
	if err := json.Unmarshal(costRR.Body.Bytes(), &cost); err != nil {
		t.Fatalf("decode cost: %v", err)
	}
	if cost["cost"].TotalUSD < 1.74 {
		t.Fatalf("cost summary = %#v", cost["cost"])
	}

	if rr := postJSON(
		"/api/v1/agent-dbs/api_deployment_route/backups",
		`{"backup_id":"api_backup","status":"restore_verified","provider":"managed"}`,
	); rr.Code != http.StatusOK {
		t.Fatalf("backup status = %d body=%s", rr.Code, rr.Body.String())
	}

	backupsReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_deployment_route/backups",
		nil,
	)
	backupsRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(backupsRR, backupsReq)
	if backupsRR.Code != http.StatusOK {
		t.Fatalf("backups status = %d body=%s", backupsRR.Code, backupsRR.Body.String())
	}

	hintsReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_deployment_route/tuning-hints",
		nil,
	)
	hintsRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(hintsRR, hintsReq)
	if hintsRR.Code != http.StatusOK {
		t.Fatalf("hints status = %d body=%s", hintsRR.Code, hintsRR.Body.String())
	}
	if !bytes.Contains(hintsRR.Body.Bytes(), []byte("vector")) {
		t.Fatalf("expected vector hints, got %s", hintsRR.Body.String())
	}

	if rr := postJSON(
		"/api/v1/agent-dbs/api_deployment_route/recommendations",
		`{"recommendation_id":"api_rec","kind":"query_rewrite","title":"rewrite"}`,
	); rr.Code != http.StatusOK {
		t.Fatalf("recommendation status = %d body=%s", rr.Code, rr.Body.String())
	}
	recsReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_deployment_route/recommendations",
		nil,
	)
	recsRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(recsRR, recsReq)
	if recsRR.Code != http.StatusOK {
		t.Fatalf("recommendations status = %d body=%s", recsRR.Code, recsRR.Body.String())
	}

	cleanupReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_deployment_route/cleanup",
		nil,
	)
	cleanupRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(cleanupRR, cleanupReq)
	if cleanupRR.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d body=%s", cleanupRR.Code, cleanupRR.Body.String())
	}
}

func TestAgentDBCleanupEndpointArchivesExpiredDeployments(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_cleanup_expired")

	_, err := st.Register(ctx, agentdb.RegisterRequest{
		DeploymentID:  "api_cleanup_expired",
		TenantID:      "tenant_agentdb_api",
		AgentID:       "agent_api",
		IsolationType: "schema",
		LeaseSeconds:  60,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET lease_expires_at=now()-interval '1 hour'
		WHERE deployment_id=$1`,
		"api_cleanup_expired",
	)
	if err != nil {
		t.Fatalf("expire deployment: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-dbs/cleanup", nil)
	rr := httptest.NewRecorder()
	agentDBCleanupAllHandler(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d body=%s", rr.Code, rr.Body.String())
	}

	dep, err := st.Get(ctx, "api_cleanup_expired")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.Status != "archived" {
		t.Fatalf("status = %s, want archived", dep.Status)
	}
}

func TestAgentDBProviderAndSizeProfileEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	profileID := "api_profile_custom"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", profileID)

	providersReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/providers",
		nil,
	)
	providersRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(providersRR, providersReq)
	if providersRR.Code != http.StatusOK {
		t.Fatalf("providers status = %d body=%s", providersRR.Code, providersRR.Body.String())
	}
	if !bytes.Contains(providersRR.Body.Bytes(), []byte("databricks_lakebase")) {
		t.Fatalf("expected lakebase provider, got %s", providersRR.Body.String())
	}

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/size-profiles",
		bytes.NewReader([]byte(`{
			"profile_id":"api_profile_custom",
			"provider":"gcp_cloudsql",
			"provisioning_level":"instance",
			"name":"cloudsql custom",
			"cpu":2,
			"memory_gb":8,
			"storage_gb":32,
			"max_connections":100,
			"monthly_budget_usd":75,
			"provider_params":{"tier":"db-custom-2-8192","region":"us-central1"}
		}`)),
	)
	createRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create profile status = %d body=%s", createRR.Code, createRR.Body.String())
	}

	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/size-profiles",
		nil,
	)
	listRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list profiles status = %d body=%s", listRR.Code, listRR.Body.String())
	}
	if !bytes.Contains(listRR.Body.Bytes(), []byte(profileID)) {
		t.Fatalf("expected profile %s, got %s", profileID, listRR.Body.String())
	}

	deleteReq := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/agent-dbs/size-profiles/api_profile_custom",
		nil,
	)
	deleteRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(deleteRR, deleteReq)
	if deleteRR.Code != http.StatusOK {
		t.Fatalf("delete profile status = %d body=%s", deleteRR.Code, deleteRR.Body.String())
	}
}

func TestAgentDBRegisterStoresProviderPlan(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_lakebase_plan")

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs",
		bytes.NewReader([]byte(`{
			"deployment_id":"api_lakebase_plan",
			"tenant_id":"tenant_agentdb_api",
			"agent_id":"agent_api",
			"provider":"databricks_lakebase",
			"provisioning_level":"instance",
			"database_name":"agent_app",
			"lease_seconds":60,
			"metadata":{"workload_types":["vector"]}
		}`)),
	)
	rr := httptest.NewRecorder()
	agentDBRegisterHandler(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("register status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got agentdb.Deployment
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode deployment: %v", err)
	}
	if got.Provider != agentdb.ProviderDatabricksLakebase {
		t.Fatalf("provider = %s", got.Provider)
	}
	if got.ProvisioningStatus != "planned" {
		t.Fatalf("provisioning status = %s", got.ProvisioningStatus)
	}
	if len(got.ProvisioningPlan["commands"].([]any)) == 0 {
		t.Fatalf("expected provisioning commands, got %#v", got.ProvisioningPlan)
	}
}

func TestAgentDBProvisionExecutionEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_exec_plan")

	_, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      "api_exec_plan",
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}

	preflightReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_exec_plan/provision/preflight",
		nil,
	)
	preflightRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(preflightRR, preflightReq)
	if preflightRR.Code != http.StatusOK {
		t.Fatalf("preflight status = %d body=%s", preflightRR.Code, preflightRR.Body.String())
	}
	if !bytes.Contains(preflightRR.Body.Bytes(), []byte("preflight")) {
		t.Fatalf("expected preflight attempt, got %s", preflightRR.Body.String())
	}

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_exec_plan/provision/execute",
		nil,
	)
	executeRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(executeRR, executeReq)
	if executeRR.Code != http.StatusOK {
		t.Fatalf("execute status = %d body=%s", executeRR.Code, executeRR.Body.String())
	}
	if !bytes.Contains(executeRR.Body.Bytes(), []byte("dry run")) {
		t.Fatalf("expected dry-run output, got %s", executeRR.Body.String())
	}

	attemptsReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_exec_plan/provision/attempts",
		nil,
	)
	attemptsRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(attemptsRR, attemptsReq)
	if attemptsRR.Code != http.StatusOK {
		t.Fatalf("attempts status = %d body=%s", attemptsRR.Code, attemptsRR.Body.String())
	}
	if !bytes.Contains(attemptsRR.Body.Bytes(), []byte("execute")) {
		t.Fatalf("expected execution attempts, got %s", attemptsRR.Body.String())
	}
}

func TestAgentDBProvisionEndpointsRetryAfterStatusCheck(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_exec_status_retry")
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_exec_status_retry")

	_, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      "api_exec_status_retry",
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}
	statusReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_exec_status_retry/provision/status",
		nil,
	)
	statusRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(statusRR, statusReq)
	if statusRR.Code != http.StatusOK {
		t.Fatalf("status check = %d body=%s", statusRR.Code, statusRR.Body.String())
	}

	preflightReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_exec_status_retry/provision/preflight",
		nil,
	)
	preflightRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(preflightRR, preflightReq)
	if preflightRR.Code != http.StatusOK {
		t.Fatalf("retry preflight = %d body=%s",
			preflightRR.Code, preflightRR.Body.String())
	}

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_exec_status_retry/provision/execute",
		nil,
	)
	executeRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(executeRR, executeReq)
	if executeRR.Code != http.StatusOK {
		t.Fatalf("retry execute = %d body=%s",
			executeRR.Code, executeRR.Body.String())
	}
	if !bytes.Contains(executeRR.Body.Bytes(), []byte("dry run")) {
		t.Fatalf("expected dry-run output, got %s", executeRR.Body.String())
	}
}

func TestAgentDBProvisionLifecycleEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	for _, id := range []string{"api_lifecycle_plan", "api_lifecycle_expired"} {
		cleanupAgentDBTestRows(t, ctx, pool, id)
	}
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_lifecycle_plan")
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_lifecycle_expired")

	_, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      "api_lifecycle_plan",
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_api",
		Provider:          agentdb.ProviderGCPCloudSQL,
		ProvisioningLevel: agentdb.LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      3600,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}

	statusReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_lifecycle_plan/provision/status",
		nil,
	)
	statusRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(statusRR, statusReq)
	if statusRR.Code != http.StatusOK {
		t.Fatalf("status check = %d body=%s", statusRR.Code, statusRR.Body.String())
	}
	if !bytes.Contains(statusRR.Body.Bytes(), []byte("status_check")) {
		t.Fatalf("expected status attempt, got %s", statusRR.Body.String())
	}

	blockedReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_lifecycle_plan/provision/destroy-dry-run",
		nil,
	)
	blockedRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(blockedRR, blockedReq)
	if blockedRR.Code != http.StatusConflict {
		t.Fatalf("blocked destroy = %d body=%s", blockedRR.Code, blockedRR.Body.String())
	}

	if _, err := st.RecordBackup(ctx, "api_lifecycle_plan", agentdb.BackupRequest{
		BackupID: "api_lifecycle_backup",
		Provider: agentdb.ProviderGCPCloudSQL,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	destroyReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_lifecycle_plan/provision/destroy-dry-run",
		nil,
	)
	destroyRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(destroyRR, destroyReq)
	if destroyRR.Code != http.StatusOK {
		t.Fatalf("destroy dry-run = %d body=%s", destroyRR.Code, destroyRR.Body.String())
	}
	if !bytes.Contains(destroyRR.Body.Bytes(), []byte("destroy_dry_run")) {
		t.Fatalf("expected destroy attempt, got %s", destroyRR.Body.String())
	}

	_, err = st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      "api_lifecycle_expired",
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_api",
		Provider:          agentdb.ProviderDatabricksLakebase,
		ProvisioningLevel: agentdb.LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision expired plan: %v", err)
	}
	_, err = pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET lease_expires_at=now()-interval '1 hour'
		WHERE deployment_id=$1`, "api_lifecycle_expired")
	if err != nil {
		t.Fatalf("expire plan: %v", err)
	}
	if _, err := st.RecordBackup(ctx, "api_lifecycle_expired", agentdb.BackupRequest{
		BackupID: "api_lifecycle_expired_backup",
		Provider: agentdb.ProviderDatabricksLakebase,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup expired: %v", err)
	}
	reconcileReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/reconcile",
		nil,
	)
	reconcileRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(reconcileRR, reconcileReq)
	if reconcileRR.Code != http.StatusOK {
		t.Fatalf("reconcile = %d body=%s", reconcileRR.Code, reconcileRR.Body.String())
	}
	if !bytes.Contains(reconcileRR.Body.Bytes(), []byte("destroy_dry_run")) {
		t.Fatalf("expected reconcile destroy dry-run, got %s", reconcileRR.Body.String())
	}
}

func TestAgentDBBackupAssuranceEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_backup_assurance")
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_backup_assurance")

	_, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      "api_backup_assurance",
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      3600,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}

	checkReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_backup_assurance/backups/check",
		nil,
	)
	checkRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(checkRR, checkReq)
	if checkRR.Code != http.StatusOK {
		t.Fatalf("backup check = %d body=%s", checkRR.Code, checkRR.Body.String())
	}
	if !bytes.Contains(checkRR.Body.Bytes(), []byte("backup_check")) {
		t.Fatalf("expected backup check attempt, got %s", checkRR.Body.String())
	}
	if !bytes.Contains(checkRR.Body.Bytes(), []byte("managed_provider")) {
		t.Fatalf("expected managed provider mode, got %s", checkRR.Body.String())
	}

	drillReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_backup_assurance/backups/restore-drill-dry-run",
		nil,
	)
	drillRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(drillRR, drillReq)
	if drillRR.Code != http.StatusOK {
		t.Fatalf("restore drill = %d body=%s", drillRR.Code, drillRR.Body.String())
	}
	if !bytes.Contains(drillRR.Body.Bytes(), []byte("restore_drill_dry_run")) {
		t.Fatalf("expected restore drill attempt, got %s", drillRR.Body.String())
	}
}

func TestAgentDBRecommendationContractEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_rec_contract")
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_rec_contract")

	_, err := st.Register(ctx, agentdb.RegisterRequest{
		DeploymentID:  "api_rec_contract",
		TenantID:      "tenant_agentdb_api",
		AgentID:       "agent_api",
		IsolationType: "schema",
		LeaseSeconds:  3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := []byte(`{
		"recommendation_id":"api_rec_action",
		"kind":"query_rewrite",
		"title":"Add LIMIT",
		"detail":"Bound scan",
		"query_fingerprint":"fp-api",
		"action_type":"query_rewrite",
		"action_risk":"safe",
		"confidence":0.81,
		"agent_instructions":{"expected_change":"add LIMIT"},
		"payload":{"sql_after":"select * from t limit 10"}
	}`)
	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_rec_contract/recommendations",
		bytes.NewReader(body),
	)
	createRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create recommendation = %d body=%s", createRR.Code, createRR.Body.String())
	}
	if !bytes.Contains(createRR.Body.Bytes(), []byte("action_type")) {
		t.Fatalf("expected action metadata, got %s", createRR.Body.String())
	}

	feedbackReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_rec_contract/recommendations/api_rec_action/feedback",
		bytes.NewReader([]byte(`{
			"decision":"accepted",
			"comment":"applied by agent",
			"applied":true,
			"result":"rewrote query"
		}`)),
	)
	feedbackRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(feedbackRR, feedbackReq)
	if feedbackRR.Code != http.StatusOK {
		t.Fatalf("feedback = %d body=%s", feedbackRR.Code, feedbackRR.Body.String())
	}

	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_rec_contract/recommendations",
		nil,
	)
	listRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list recommendations = %d body=%s", listRR.Code, listRR.Body.String())
	}
	for _, want := range []string{"accepted", "rewrote query", "sql_after", "safe"} {
		if !bytes.Contains(listRR.Body.Bytes(), []byte(want)) {
			t.Fatalf("expected %q in recommendation list, got %s", want, listRR.Body.String())
		}
	}
}

func TestAgentDBAuditEndpointsExposeEventsAndJSONL(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_audit_contract")
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_audit WHERE deployment_id=$1",
		"api_audit_contract",
	)
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_audit_contract")

	_, err := st.Register(ctx, agentdb.RegisterRequest{
		DeploymentID:  "api_audit_contract",
		TenantID:      "tenant_agentdb_api",
		AgentID:       "agent_api",
		IsolationType: "schema",
		LeaseSeconds:  3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := st.UpsertRecommendation(ctx, "api_audit_contract",
		agentdb.RecommendationCreate{
			RecommendationID: "api_audit_rec",
			Kind:             "query_rewrite",
			Title:            "Bound query",
		}); err != nil {
		t.Fatalf("UpsertRecommendation: %v", err)
	}
	if err := st.Feedback(ctx, "api_audit_contract", "api_audit_rec",
		agentdb.FeedbackRequest{
			Decision: "accepted",
			Applied:  true,
			Result:   "agent applied SQL",
		}); err != nil {
		t.Fatalf("Feedback: %v", err)
	}

	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_audit_contract/audit",
		nil,
	)
	listRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("audit list = %d body=%s", listRR.Code, listRR.Body.String())
	}
	for _, want := range []string{
		"audit_events", "register", "recommendation_feedback",
	} {
		if !bytes.Contains(listRR.Body.Bytes(), []byte(want)) {
			t.Fatalf("expected %q in audit list, got %s",
				want, listRR.Body.String())
		}
	}

	exportReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_audit_contract/audit/export",
		nil,
	)
	exportRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(exportRR, exportReq)
	if exportRR.Code != http.StatusOK {
		t.Fatalf("audit export = %d body=%s", exportRR.Code, exportRR.Body.String())
	}
	if got := exportRR.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content type = %q", got)
	}
	if !bytes.Contains(exportRR.Body.Bytes(), []byte(`"deployment_id":"api_audit_contract"`)) ||
		!bytes.Contains(exportRR.Body.Bytes(), []byte(`"event":"recommendation_feedback"`)) {
		t.Fatalf("unexpected JSONL body: %s", exportRR.Body.String())
	}
}

func TestAgentDBDeployRequestEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_deploy_request")
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_deploy_request")

	_, err := st.Register(ctx, agentdb.RegisterRequest{
		DeploymentID:  "api_deploy_request",
		TenantID:      "tenant_agentdb_api",
		AgentID:       "agent_api",
		RunID:         "run_api",
		IsolationType: "schema",
		LeaseSeconds:  3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_deploy_request/deploy-requests",
		bytes.NewReader([]byte(`{
			"deploy_request_id":"api_dr",
			"title":"Promote agent schema",
			"reason":"ready for production review",
			"risk_tier":"moderate",
			"migration_sql":"CREATE TABLE prod.agent_items(id bigint primary key);",
			"verification_sql":"SELECT count(*) FROM prod.agent_items;",
			"rollback_sql":"DROP TABLE prod.agent_items;",
			"status":"review_requested",
			"created_by":"operator"
		}`)),
	)
	createRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create deploy request = %d body=%s", createRR.Code, createRR.Body.String())
	}
	for _, want := range []string{"api_dr", "review_requested", "review_only"} {
		if !bytes.Contains(createRR.Body.Bytes(), []byte(want)) {
			t.Fatalf("expected %q in create response, got %s", want, createRR.Body.String())
		}
	}

	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_deploy_request/deploy-requests",
		nil,
	)
	listRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list deploy requests = %d body=%s", listRR.Code, listRR.Body.String())
	}
	if !bytes.Contains(listRR.Body.Bytes(), []byte("deploy_requests")) ||
		!bytes.Contains(listRR.Body.Bytes(), []byte("api_dr")) {
		t.Fatalf("unexpected list response: %s", listRR.Body.String())
	}

	approveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_deploy_request/deploy-requests/api_dr/approve",
		bytes.NewReader([]byte(`{
			"reviewed_by":"dba",
			"review_reason":"migration checked"
		}`)),
	)
	approveRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(approveRR, approveReq)
	if approveRR.Code != http.StatusOK {
		t.Fatalf("approve deploy request = %d body=%s", approveRR.Code, approveRR.Body.String())
	}
	if !bytes.Contains(approveRR.Body.Bytes(), []byte(`"status":"approved"`)) {
		t.Fatalf("expected approved response, got %s", approveRR.Body.String())
	}

	wrongReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_other/deploy-requests/api_dr/deny",
		bytes.NewReader([]byte(`{
			"reviewed_by":"dba",
			"review_reason":"wrong deployment"
		}`)),
	)
	wrongRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(wrongRR, wrongReq)
	if wrongRR.Code != http.StatusNotFound {
		t.Fatalf("wrong-scope deny = %d body=%s", wrongRR.Code, wrongRR.Body.String())
	}
}

func TestAgentDBIdentityAndPingTokenEndpoints(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	cleanupAgentDBTestRows(t, ctx, pool, "api_identity_token")
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_identities WHERE agent_id=$1", "agent_api_token")
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_ping_token_failures WHERE deployment_id=$1", "api_identity_token")
	defer cleanupAgentDBTestRows(t, ctx, pool, "api_identity_token")

	createIdentityReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/identities",
		bytes.NewReader([]byte(`{
			"agent_id":"agent_api_token",
			"tenant_id":"tenant_agentdb_api",
			"owner_id":"owner_api",
			"display_name":"API token agent",
			"metadata":{"framework":"test"}
		}`)),
	)
	createIdentityRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(createIdentityRR, createIdentityReq)
	if createIdentityRR.Code != http.StatusOK {
		t.Fatalf("create identity = %d body=%s",
			createIdentityRR.Code, createIdentityRR.Body.String())
	}

	listIdentityReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/identities",
		nil,
	)
	listIdentityRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listIdentityRR, listIdentityReq)
	if listIdentityRR.Code != http.StatusOK ||
		!bytes.Contains(listIdentityRR.Body.Bytes(), []byte("agent_api_token")) {
		t.Fatalf("list identities = %d body=%s",
			listIdentityRR.Code, listIdentityRR.Body.String())
	}

	_, err := st.Register(ctx, agentdb.RegisterRequest{
		DeploymentID:  "api_identity_token",
		TenantID:      "tenant_agentdb_api",
		AgentID:       "agent_api_token",
		IsolationType: "schema",
		LeaseSeconds:  60,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	tokenReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_identity_token/ping-tokens",
		bytes.NewReader([]byte(`{
			"agent_id":"agent_api_token",
			"expires_seconds":3600
		}`)),
	)
	tokenRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(tokenRR, tokenReq)
	if tokenRR.Code != http.StatusOK {
		t.Fatalf("create ping token = %d body=%s", tokenRR.Code, tokenRR.Body.String())
	}
	var token agentdb.PingToken
	if err := json.Unmarshal(tokenRR.Body.Bytes(), &token); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if token.Token == "" || bytes.Contains(tokenRR.Body.Bytes(), []byte("token_hash")) {
		t.Fatalf("token response leaked or omitted secret: %s", tokenRR.Body.String())
	}

	listTokenReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/api_identity_token/ping-tokens",
		nil,
	)
	listTokenRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listTokenRR, listTokenReq)
	if listTokenRR.Code != http.StatusOK {
		t.Fatalf("list ping tokens = %d body=%s",
			listTokenRR.Code, listTokenRR.Body.String())
	}
	if bytes.Contains(listTokenRR.Body.Bytes(), []byte(token.Token)) ||
		bytes.Contains(listTokenRR.Body.Bytes(), []byte("token_hash")) {
		t.Fatalf("list ping tokens leaked secret material: %s", listTokenRR.Body.String())
	}

	pingReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_identity_token/agent-ping",
		bytes.NewReader([]byte(`{"status":"active","metrics":{"qps":1}}`)),
	)
	pingReq.Header.Set("Authorization", "Bearer "+token.Token)
	pingRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(pingRR, pingReq)
	if pingRR.Code != http.StatusOK {
		t.Fatalf("agent ping = %d body=%s", pingRR.Code, pingRR.Body.String())
	}
	if !bytes.Contains(pingRR.Body.Bytes(), []byte("last_ping_at")) {
		t.Fatalf("expected pinged deployment, got %s", pingRR.Body.String())
	}

	if !shouldSkipAuth("/api/v1/agent-dbs/api_identity_token/agent-ping") {
		t.Fatalf("agent-ping should bypass session auth and rely on ping token")
	}

	rotateReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_identity_token/ping-tokens/"+token.TokenID+"/rotate",
		bytes.NewReader([]byte(`{"expires_seconds":7200}`)),
	)
	rotateRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(rotateRR, rotateReq)
	if rotateRR.Code != http.StatusOK {
		t.Fatalf("rotate ping token = %d body=%s", rotateRR.Code, rotateRR.Body.String())
	}
	var rotated agentdb.PingToken
	if err := json.Unmarshal(rotateRR.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotated token: %v", err)
	}
	if rotated.Token == "" || rotated.RotatedFromTokenID != token.TokenID {
		t.Fatalf("rotated token = %#v", rotated)
	}

	mux := http.NewServeMux()
	registerAgentDBRoutes(mux, st)
	directPingReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_identity_token/agent-ping",
		bytes.NewReader([]byte(`{"status":"active"}`)),
	)
	directPingReq.Header.Set("Authorization", "Bearer "+rotated.Token)
	directPingRR := httptest.NewRecorder()
	mux.ServeHTTP(directPingRR, directPingReq)
	if directPingRR.Code != http.StatusOK {
		t.Fatalf("direct agent ping route = %d body=%s",
			directPingRR.Code, directPingRR.Body.String())
	}

	revokeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/api_identity_token/ping-tokens/"+rotated.TokenID+"/revoke",
		bytes.NewReader([]byte(`{"reason":"operator rotation"}`)),
	)
	revokeRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(revokeRR, revokeReq)
	if revokeRR.Code != http.StatusOK {
		t.Fatalf("revoke ping token = %d body=%s", revokeRR.Code, revokeRR.Body.String())
	}
	if !bytes.Contains(revokeRR.Body.Bytes(), []byte(`"status":"revoked"`)) ||
		bytes.Contains(revokeRR.Body.Bytes(), []byte(rotated.Token)) {
		t.Fatalf("revoke response should be revoked and redacted: %s", revokeRR.Body.String())
	}

	for i := 0; i < 5; i++ {
		badReq := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/agent-dbs/api_identity_token/agent-ping",
			bytes.NewReader([]byte(`{"status":"active"}`)),
		)
		badReq.Header.Set("Authorization", "Bearer bad-token")
		badRR := httptest.NewRecorder()
		agentDBSubrouter(st).ServeHTTP(badRR, badReq)
		if i < 4 && badRR.Code != http.StatusNotFound {
			t.Fatalf("bad token attempt %d = %d body=%s", i, badRR.Code, badRR.Body.String())
		}
		if i == 4 && badRR.Code != http.StatusTooManyRequests {
			t.Fatalf("rate-limited token attempt = %d body=%s",
				badRR.Code, badRR.Body.String())
		}
	}
}

func TestAgentDBErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
	}{
		{"not found", agentdb.ErrNotFound, http.StatusNotFound},
		{"invalid", agentdb.ErrInvalid, http.StatusBadRequest},
		{"conflict", agentdb.ErrConflict, http.StatusConflict},
		{"restore required", agentdb.ErrRestoreRequired, http.StatusConflict},
		{"delete blocked", agentdb.ErrDeleteBlocked, http.StatusConflict},
		{"rate limited", agentdb.ErrRateLimited, http.StatusTooManyRequests},
		{"generic", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			agentDBError(rr, tt.err)
			if rr.Code != tt.code {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, tt.code, rr.Body.String())
			}
		})
	}
}

func TestAgentDBSubrouterRejectsUnknownPaths(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/deployment/unknown-path",
		nil,
	)
	rr := httptest.NewRecorder()

	agentDBSubrouter(agentdb.NewStore(nil)).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAgentDBProviderConfigAPI(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_provider_configs WHERE provider=$1",
		agentdb.ProviderAWSRDS,
	)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/provider-configs/aws_rds",
		bytes.NewReader([]byte(`{
			"enabled": true,
			"settings": {"allowed_regions": ["us-east-1"], "max_ttl_seconds": 3600}
		}`)),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upsert status = %d body=%s", rr.Code, rr.Body.String())
	}
	var cfg agentdb.ProviderConfig
	if err := json.NewDecoder(rr.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode provider config: %v", err)
	}
	if cfg.Provider != agentdb.ProviderAWSRDS || !cfg.Enabled {
		t.Fatalf("provider config = %#v", cfg)
	}

	badReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/provider-configs/aws_rds",
		bytes.NewReader([]byte(`{
			"enabled": true,
			"settings": {"secret_token": "do-not-store"}
		}`)),
	)
	badRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusBadRequest {
		t.Fatalf("secret settings status = %d body=%s", badRR.Code, badRR.Body.String())
	}

	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/provider-configs",
		nil,
	)
	listRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listRR.Code, listRR.Body.String())
	}
	if !strings.Contains(listRR.Body.String(), agentdb.ProviderAWSRDS) {
		t.Fatalf("list missing provider: %s", listRR.Body.String())
	}
}

func TestAgentDBTerraformTemplateAPI(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	deploymentID := "tf_api_unit_dep"
	cleanupAgentDBTestRows(t, ctx, pool, deploymentID)
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1",
		"tf_api_unit",
	)
	body := []byte(`{
		"template_id":"tf_api_unit",
		"name":"api unit",
		"files":[{"path":"main.tf","body":"resource \"aws_db_instance\" \"db\" {}"}],
		"created_by":"api-test"
	}`)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/terraform-templates",
		bytes.NewReader(body),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create template status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created agentdb.TerraformTemplate
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode template: %v", err)
	}
	if created.TemplateID != "tf_api_unit" || created.Status != "draft" {
		t.Fatalf("created = %#v", created)
	}
	approveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/terraform-templates/tf_api_unit/approve",
		bytes.NewReader([]byte(`{"approved_by":"operator"}`)),
	)
	approveRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(approveRR, approveReq)
	if approveRR.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", approveRR.Code, approveRR.Body.String())
	}
	provisionReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/terraform-templates/tf_api_unit/provision",
		bytes.NewReader([]byte(`{
			"deployment_id":"tf_api_unit_dep",
			"tenant_id":"tenant_agentdb_api",
			"agent_id":"agent_tf_api",
			"provider":"aws_rds",
			"provisioning_level":"instance",
			"lease_seconds":3600,
			"budget_usd":20,
			"provider_params":{"region":"us-east-1"}
		}`)),
	)
	provisionRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(provisionRR, provisionReq)
	if provisionRR.Code != http.StatusOK {
		t.Fatalf("provision template status = %d body=%s",
			provisionRR.Code, provisionRR.Body.String())
	}
	var dep agentdb.Deployment
	if err := json.NewDecoder(provisionRR.Body).Decode(&dep); err != nil {
		t.Fatalf("decode provisioned deployment: %v", err)
	}
	if dep.DeploymentID != deploymentID ||
		dep.Metadata["terraform_template_id"] != "tf_api_unit" ||
		dep.ProvisioningStatus != "planned" {
		t.Fatalf("deployment = %#v", dep)
	}
	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/terraform-templates",
		nil,
	)
	listRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK ||
		!strings.Contains(listRR.Body.String(), "tf_api_unit") {
		t.Fatalf("list status = %d body=%s", listRR.Code, listRR.Body.String())
	}
}

func TestAgentDBTerraformTemplateAPIRejectsDangerousTemplate(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1",
		"tf_api_bad",
	)
	body := []byte(`{
		"template_id":"tf_api_bad",
		"name":"bad",
		"files":[{"path":"main.tf","body":"resource \"null_resource\" \"x\" {}"}],
		"created_by":"api-test"
	}`)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/terraform-templates",
		bytes.NewReader(body),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create rejected-template status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created agentdb.TerraformTemplate
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode rejected template: %v", err)
	}
	if created.Status != "rejected" || len(created.PolicyFindings) == 0 {
		t.Fatalf("created = %#v", created)
	}
	approveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/terraform-templates/tf_api_bad/approve",
		bytes.NewReader([]byte(`{"approved_by":"operator"}`)),
	)
	approveRR := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(approveRR, approveReq)
	if approveRR.Code != http.StatusBadRequest {
		t.Fatalf("approve status = %d body=%s", approveRR.Code, approveRR.Body.String())
	}
}

func TestAgentDBRequestApprovalProvisionAPI(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	requestID := "req_api_provision"
	deploymentID := "req_api_provision_dep"
	cleanupAgentDBTestRows(t, ctx, pool, requestID)
	cleanupAgentDBTestRows(t, ctx, pool, deploymentID)
	handler := agentDBSubrouterWithRegistry(
		st,
		agentdb.DefaultRunnerRegistry(),
		apiStaticBlueprintGenerator{spec: agentdb.BlueprintSpec{
			Provider:            agentdb.ProviderAWSRDS,
			ProvisioningLevel:   agentdb.LevelInstance,
			Region:              "us-east-1",
			InstanceClass:       "db.t4g.micro",
			StorageGB:           20,
			BackupRetentionDays: 7,
			PITR:                true,
			MultiAZ:             true,
			PrivateNetwork:      true,
			Extensions:          []string{"pgvector"},
		}},
	)

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/requests",
		bytes.NewReader([]byte(`{
			"request_id":"req_api_provision",
			"tenant_id":"tenant_agentdb_api",
			"agent_id":"agent_request_api",
			"purpose":"temporary review app database",
			"requested_isolation_type":"instance",
			"provider":"aws_rds",
			"database_name":"request_api",
			"backup_required":true
		}`)),
	)
	createRR := httptest.NewRecorder()
	handler.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create request status = %d body=%s",
			createRR.Code, createRR.Body.String())
	}
	var created agentdb.Request
	if err := json.NewDecoder(createRR.Body).Decode(&created); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if created.Status != "requested" || created.Provider != agentdb.ProviderAWSRDS {
		t.Fatalf("created request = %#v", created)
	}

	approveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/requests/req_api_provision/approve",
		bytes.NewReader([]byte(`{"reason":"operator approved disposable cloud DB"}`)),
	)
	approveRR := httptest.NewRecorder()
	handler.ServeHTTP(approveRR, approveReq)
	if approveRR.Code != http.StatusOK {
		t.Fatalf("approve request status = %d body=%s",
			approveRR.Code, approveRR.Body.String())
	}

	provisionReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/requests/req_api_provision/provision",
		bytes.NewReader([]byte(`{
			"deployment_id":"req_api_provision_dep",
			"lease_seconds":3600,
			"metadata":{"source":"agent_api_test"}
		}`)),
	)
	provisionRR := httptest.NewRecorder()
	handler.ServeHTTP(provisionRR, provisionReq)
	if provisionRR.Code != http.StatusOK {
		t.Fatalf("provision request status = %d body=%s",
			provisionRR.Code, provisionRR.Body.String())
	}
	var dep agentdb.Deployment
	if err := json.NewDecoder(provisionRR.Body).Decode(&dep); err != nil {
		t.Fatalf("decode deployment: %v", err)
	}
	if dep.DeploymentID != deploymentID ||
		dep.Metadata["request_id"] != requestID ||
		dep.ProvisioningStatus != "planned" {
		t.Fatalf("deployment = %#v", dep)
	}
}

func TestAgentDBBlueprintAPI(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	id := "bp_api_unit"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")

	handler := agentDBSubrouterWithRegistry(
		st,
		agentdb.DefaultRunnerRegistry(),
		apiStaticBlueprintGenerator{spec: agentdb.BlueprintSpec{
			Provider:            agentdb.ProviderAWSRDS,
			ProvisioningLevel:   agentdb.LevelInstance,
			Region:              "us-east-1",
			InstanceClass:       "db.t4g.micro",
			StorageGB:           20,
			BackupRetentionDays: 7,
			PITR:                true,
			MultiAZ:             true,
			PrivateNetwork:      true,
			Extensions:          []string{"pgvector"},
		}},
	)
	body := `{
		"blueprint_id":"bp_api_unit",
		"name":"API unit",
		"provider":"aws_rds",
		"intent":"Private AWS RDS in us-east-1 with Multi-AZ, PITR, 7-day backups and pgvector.",
		"created_by":"api-test"
	}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/blueprints",
		strings.NewReader(body),
	)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create blueprint status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created agentdb.Blueprint
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode blueprint: %v", err)
	}
	if created.BlueprintID != id || created.TemplateID != id+"_tf" {
		t.Fatalf("created = %#v", created)
	}
	if created.Status != "generated" || created.Spec.Provider != agentdb.ProviderAWSRDS {
		t.Fatalf("status/spec = %s/%#v", created.Status, created.Spec)
	}

	listRR := httptest.NewRecorder()
	handler.ServeHTTP(listRR, httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agent-dbs/blueprints",
		nil,
	))
	if listRR.Code != http.StatusOK {
		t.Fatalf("list blueprint status = %d body=%s", listRR.Code, listRR.Body.String())
	}
	var listed struct {
		Blueprints []agentdb.Blueprint `json:"blueprints"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Blueprints) == 0 || listed.Blueprints[0].BlueprintID == "" {
		t.Fatalf("listed = %#v", listed)
	}
}

func TestAgentDBBlueprintAPIRequiresLLMGenerator(t *testing.T) {
	st, _, pool := requireAgentDBAPIStore(t)
	defer pool.Close()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/blueprints",
		strings.NewReader(`{
			"blueprint_id":"bp_api_requires_llm",
			"name":"API requires LLM",
			"provider":"aws_rds",
			"intent":"Create AWS RDS in us central 1."
		}`),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "blueprint generation requires llm") {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestAgentDBBlueprintToLiveProvisioningAPI(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	id := "api_blueprint_live"
	cleanupAgentDBTestRows(t, ctx, pool, id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")
	defer cleanupAgentDBTestRows(t, ctx, pool, id)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")

	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(apiFakeProviderRunner{})
	router := agentDBSubrouterWithRegistry(
		st,
		registry,
		apiStaticBlueprintGenerator{spec: agentdb.BlueprintSpec{
			Provider:            agentdb.ProviderAWSRDS,
			ProvisioningLevel:   agentdb.LevelInstance,
			Region:              "us-east-2",
			InstanceClass:       "db.t4g.micro",
			StorageGB:           20,
			BackupRetentionDays: 1,
			PrivateNetwork:      true,
		}},
	)
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	if rr := post("/api/v1/agent-dbs/blueprints", `{
		"blueprint_id":"api_blueprint_live",
		"name":"API blueprint live",
		"provider":"aws_rds",
		"intent":"Private AWS RDS Postgres in us-east-2 with 20GB storage and 1-day backups."
	}`); rr.Code != http.StatusOK {
		t.Fatalf("create blueprint status = %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := post(
		"/api/v1/agent-dbs/blueprints/api_blueprint_live/approve",
		`{"approved_by":"operator"}`,
	); rr.Code != http.StatusOK {
		t.Fatalf("approve blueprint status = %d body=%s", rr.Code, rr.Body.String())
	}
	provisionRR := post(
		"/api/v1/agent-dbs/blueprints/api_blueprint_live/provision",
		`{"deployment_id":"api_blueprint_live","tenant_id":"tenant_agentdb_api","agent_id":"agent_api","lease_seconds":3600}`,
	)
	if provisionRR.Code != http.StatusOK {
		t.Fatalf("provision blueprint status = %d body=%s", provisionRR.Code, provisionRR.Body.String())
	}
	if rr := post("/api/v1/agent-dbs/api_blueprint_live/provision/preflight", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("preflight status = %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := post(
		"/api/v1/agent-dbs/api_blueprint_live/provision/execute",
		`{"mode":"live","live_enabled":true,"provider_enabled":true,"cost_estimate_id":"estimate-api"}`,
	); rr.Code != http.StatusOK {
		t.Fatalf("live execute status = %d body=%s", rr.Code, rr.Body.String())
	} else if !strings.Contains(rr.Body.String(), `"kind":"execute_live"`) {
		t.Fatalf("live execute used wrong path body=%s", rr.Body.String())
	}
	if rr := post("/api/v1/agent-dbs/api_blueprint_live/provision/status", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("live status status = %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := post("/api/v1/agent-dbs/api_blueprint_live/backups/check", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("backup check status = %d body=%s", rr.Code, rr.Body.String())
	} else if !strings.Contains(rr.Body.String(), `"safe_for_destroy":true`) {
		t.Fatalf("backup check did not verify restore body=%s", rr.Body.String())
	}
	if rr := post("/api/v1/agent-dbs/api_blueprint_live/provision/destroy-live", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("live destroy status = %d body=%s", rr.Code, rr.Body.String())
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.Metadata["blueprint_id"] != id || !dep.LiveMode {
		t.Fatalf("deployment after gauntlet = %#v", dep)
	}
}

func TestAgentDBBlueprintLLMGeneratorParsesJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		content := "```json\n" +
			`{"provider":"aws_rds","provisioning_level":"instance",` +
			`"region":"us-west-2","instance_class":"db.t4g.small",` +
			`"storage_gb":50,"backup_retention_days":7,"pitr":true,` +
			`"multi_az":true,"private_network":true,"public_ip":false,` +
			`"extensions":["pgvector"],"budget_usd":250,` +
			`"tags":{"managed_by":"pg_sage"}}` + "\n```"
		resp := map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"total_tokens": 42},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 1000,
	}, func(string, string, ...any) {})
	gen := llmBlueprintGenerator{
		client: client,
	}
	out, err := gen.GenerateBlueprint(context.Background(), agentdb.BlueprintDraftRequest{
		Provider: agentdb.ProviderAWSRDS,
		Intent:   "Create a private RDS database.",
	})
	if err != nil {
		t.Fatalf("GenerateBlueprint: %v", err)
	}
	if !out.LLMUsed {
		t.Fatalf("expected LLM path, got %#v", out)
	}
	if out.Spec.Region != "us-west-2" || out.Spec.StorageGB != 50 {
		t.Fatalf("spec = %#v", out.Spec)
	}
	if len(out.Files) != 1 || !strings.Contains(out.Files[0].Body, "aws_db_instance") {
		t.Fatalf("files = %#v", out.Files)
	}
}

func TestAgentDBBlueprintLLMGeneratorDoesNotFallbackToHeuristics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": `{"provider":"aws_rds"`},
				"finish_reason": "stop",
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 1000,
	}, func(string, string, ...any) {})
	gen := llmBlueprintGenerator{client: client}
	_, err := gen.GenerateBlueprint(context.Background(), agentdb.BlueprintDraftRequest{
		Provider: agentdb.ProviderAWSRDS,
		Intent:   "$200 a month ha instance in us central 1",
	})
	if !errors.Is(err, agentdb.ErrBlueprintLLMRequired) {
		t.Fatalf("err = %v, want ErrBlueprintLLMRequired", err)
	}
}

func TestAgentDBBlueprintLLMGeneratorRequiresCloudRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		content := `{"provider":"aws_rds","provisioning_level":"instance",` +
			`"instance_class":"db.t4g.small","storage_gb":20,` +
			`"backup_retention_days":7,"private_network":true}`
		resp := map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 1000,
	}, func(string, string, ...any) {})
	gen := llmBlueprintGenerator{client: client}
	_, err := gen.GenerateBlueprint(context.Background(), agentdb.BlueprintDraftRequest{
		Provider: agentdb.ProviderAWSRDS,
		Intent:   "Create a private RDS database in us-east-2.",
	})
	if !errors.Is(err, agentdb.ErrBlueprintLLMRequired) {
		t.Fatalf("err = %v, want ErrBlueprintLLMRequired", err)
	}
}

func TestAgentDBLiveProvisionAPI(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	id := "api_live_provision"
	cleanupAgentDBTestRows(t, ctx, pool, id)
	if _, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_live_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(apiFakeProviderRunner{})
	disabledReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/"+id+"/provision/execute",
		bytes.NewReader([]byte(`{"mode":"live"}`)),
	)
	disabledRR := httptest.NewRecorder()
	agentDBSubrouterWithRegistry(st, registry, nil).ServeHTTP(disabledRR, disabledReq)
	if disabledRR.Code != http.StatusBadRequest {
		t.Fatalf("disabled live status = %d", disabledRR.Code)
	}
	liveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/"+id+"/provision/execute",
		bytes.NewReader([]byte(`{
			"mode":"live",
			"live_enabled":true,
			"provider_enabled":true,
			"cost_estimate_id":"estimate-api"
		}`)),
	)
	liveRR := httptest.NewRecorder()
	agentDBSubrouterWithRegistry(st, registry, nil).ServeHTTP(liveRR, liveReq)
	if liveRR.Code != http.StatusOK {
		t.Fatalf("live execute status = %d body=%s", liveRR.Code, liveRR.Body.String())
	}
	var attempt agentdb.ProvisionAttempt
	if err := json.NewDecoder(liveRR.Body).Decode(&attempt); err != nil {
		t.Fatalf("decode live attempt: %v", err)
	}
	if attempt.Kind != "execute_live" || attempt.Runner != "api_fake_runner" {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func TestAgentDBLiveProvisionAPIRejectsMissingCostEstimate(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	id := "api_live_missing_cost"
	cleanupAgentDBTestRows(t, ctx, pool, id)
	if _, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_live_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(apiFakeProviderRunner{})
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/"+id+"/provision/execute",
		bytes.NewReader([]byte(`{
			"mode":"live",
			"live_enabled":true,
			"provider_enabled":true
		}`)),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouterWithRegistry(st, registry, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAgentDBLiveProvisionAPIReportsUnavailableRunner(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	id := "api_live_runner_unavailable"
	cleanupAgentDBTestRows(t, ctx, pool, id)
	defer cleanupAgentDBTestRows(t, ctx, pool, id)
	if _, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_live_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/"+id+"/provision/execute",
		bytes.NewReader([]byte(`{
			"mode":"live",
			"live_enabled":true,
			"provider_enabled":true,
			"cost_estimate_id":"estimate-api"
		}`)),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouterWithRegistry(
		st,
		agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{}),
		nil,
	).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "live provider runner unavailable") {
		t.Fatalf("expected unavailable runner message, got %s", rr.Body.String())
	}
}

func TestAgentDBLiveProvisionAPIPromotesDryRunReadyDeployment(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	id := "api_live_after_dry_run"
	cleanupAgentDBTestRows(t, ctx, pool, id)
	defer cleanupAgentDBTestRows(t, ctx, pool, id)
	if _, err := st.Provision(ctx, agentdb.RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_api",
		AgentID:           "agent_live_api",
		Provider:          agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	if _, err := st.ExecuteProvision(ctx, id, &agentdb.DryRunProvisionRunner{}); err != nil {
		t.Fatalf("ExecuteProvision dry-run: %v", err)
	}
	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(apiFakeProviderRunner{})
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent-dbs/"+id+"/provision/execute",
		bytes.NewReader([]byte(`{
			"mode":"live",
			"live_enabled":true,
			"provider_enabled":true,
			"cost_estimate_id":"estimate-api"
		}`)),
	)
	rr := httptest.NewRecorder()
	agentDBSubrouterWithRegistry(st, registry, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("live execute status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"kind":"execute_live"`) {
		t.Fatalf("expected live attempt, got %s", rr.Body.String())
	}
}

func TestAgentDBHelperParsers(t *testing.T) {
	m := map[string]any{
		"i_float": 7.8,
		"i_text":  "42",
		"f_int":   5,
		"f_text":  "3.5",
		"flag":    true,
		"obj":     map[string]any{"ok": true},
	}

	if integer(m, "i_float") != 7 || integer(m, "i_text") != 42 {
		t.Fatalf("integer parser failed")
	}
	if float(m, "f_int") != 5 || float(m, "f_text") != 3.5 {
		t.Fatalf("float parser failed")
	}
	if !boolValue(m, "flag") {
		t.Fatalf("bool parser failed")
	}
	if obj(m, "obj")["ok"] != true {
		t.Fatalf("object parser failed")
	}
	if firstString("", "winner", "ignored") != "winner" {
		t.Fatalf("firstString parser failed")
	}
}

func cleanupAgentDBTestRows(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	id string,
) {
	t.Helper()
	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE request_id=$1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_ping_token_failures WHERE deployment_id=$1", id)
	}
	cleanup()
	t.Cleanup(cleanup)
}

func requireAgentDBAPIStore(t *testing.T) (*agentdb.Store, context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unavailable: %v", err)
	}
	st := agentdb.NewStore(pool)
	if err := st.Ensure(ctx); err != nil {
		pool.Close()
		t.Fatalf("ensure schema: %v", err)
	}
	return st, ctx, pool
}

type apiFakeProviderRunner struct{}

func (apiFakeProviderRunner) Name() string { return "api_fake_runner" }

func (apiFakeProviderRunner) Provider() string { return agentdb.ProviderAWSRDS }

func (apiFakeProviderRunner) Preflight(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "preflight_passed"}
}

func (apiFakeProviderRunner) Create(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{
		Status:             "available",
		ProviderResourceID: "api-live-resource",
		Detail:             map[string]any{"state": "available"},
	}
}

func (apiFakeProviderRunner) Status(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "available"}
}

func (apiFakeProviderRunner) Destroy(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "destroying"}
}

func (apiFakeProviderRunner) BackupCheck(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "verified"}
}

type apiStaticBlueprintGenerator struct {
	spec agentdb.BlueprintSpec
}

func (g apiStaticBlueprintGenerator) GenerateBlueprint(
	_ context.Context,
	_ agentdb.BlueprintDraftRequest,
) (agentdb.BlueprintGeneration, error) {
	spec := agentdb.NormalizeBlueprintSpec(g.spec, "")
	files, err := agentdb.RenderTerraformFromBlueprint(spec)
	if err != nil {
		return agentdb.BlueprintGeneration{}, err
	}
	return agentdb.BlueprintGeneration{
		Spec:    spec,
		Files:   files,
		LLMUsed: true,
	}, nil
}
