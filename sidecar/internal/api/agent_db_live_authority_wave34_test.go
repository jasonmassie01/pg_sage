package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestWave34LiveAuthorityIssueIsAdminPersistedAndIdempotent(t *testing.T) {
	fixture := newWave34AuthorityFixture(t, "wave34_authority_issue")
	path := fixture.authorizePath()
	body := map[string]any{"operation": "create", "idempotency_key": "create-1"}

	assertWave34IssueDenied(t, fixture.handler, path, body, nil, http.StatusUnauthorized)
	assertWave34IssueDenied(
		t, fixture.handler, path, body, testOperatorUser(), http.StatusForbidden,
	)
	first := issueWave34Authorization(t, fixture, "create", "create-1")
	second := issueWave34Authorization(t, fixture, "create", "create-1")
	assertWave34SameTuple(t, first, second)
	assertWave34ServerSummary(t, first, fixture.id, agentdb.ProvisionOpCreate)
	assertWave34PersistedTuple(t, fixture, first, agentdb.ProvisionOpCreate)
}

func TestWave34LiveExecuteUsesOnlyIssuedTupleAndExactReplay(t *testing.T) {
	fixture := newWave34AuthorityFixture(t, "wave34_authority_execute")
	tuple := issueWave34Authorization(t, fixture, "create", "execute-1")

	claimsOnly := wave34ClientClaims()
	assertWave34ExecuteStatus(t, fixture, claimsOnly, http.StatusBadRequest)
	if got := fixture.runner.createCalls.Load(); got != 0 {
		t.Fatalf("claims-only provider calls = %d, want 0", got)
	}
	assertWave34WrongActorRejected(t, fixture, tuple)
	assertWave34MismatchedTupleConflicts(t, fixture, tuple)

	body := wave34TupleBody(tuple)
	for key, value := range wave34ClientClaims() {
		body[key] = value
	}
	first := assertWave34ExecuteStatus(t, fixture, body, http.StatusOK)
	second := assertWave34ExecuteStatus(t, fixture, body, http.StatusOK)
	if got := fixture.runner.createCalls.Load(); got != 1 {
		t.Fatalf("provider create calls after exact replay = %d, want 1", got)
	}
	assertWave34ReceiptPersisted(t, fixture, tuple)
	if first.Body.Len() == 0 || second.Body.Len() == 0 {
		t.Fatal("live execution and replay must both return a response")
	}
}

func TestWave34LiveDestroyRequiresSeparateExactAuthorization(t *testing.T) {
	fixture := newWave34AuthorityFixture(t, "wave34_authority_destroy")
	createTuple := issueWave34Authorization(t, fixture, "create", "create-before-destroy")
	fixture.promoteToLive(t)

	createBody := wave34TupleBody(createTuple)
	assertWave34DestroyStatus(t, fixture, createBody, http.StatusConflict)
	if got := fixture.runner.destroyCalls.Load(); got != 0 {
		t.Fatalf("destroy calls with create authorization = %d, want 0", got)
	}
	destroyTuple := issueWave34Authorization(t, fixture, "destroy", "destroy-1")
	if destroyTuple.AuthorizationID == createTuple.AuthorizationID ||
		destroyTuple.PlanHash == createTuple.PlanHash {
		t.Fatalf("destroy reused create authority: create=%+v destroy=%+v",
			createTuple, destroyTuple)
	}
	assertWave34ServerSummary(t, destroyTuple, fixture.id, agentdb.ProvisionOpDestroy)
	assertWave34DestroyStatus(t, fixture, wave34TupleBody(destroyTuple), http.StatusOK)
	if got := fixture.runner.destroyCalls.Load(); got != 1 {
		t.Fatalf("provider destroy calls = %d, want 1", got)
	}
}

func newWave34AuthorityFixture(t *testing.T, id string) *wave34AuthorityFixture {
	t.Helper()
	st, ctx, pool := requireAgentDBAPIStore(t)
	t.Cleanup(pool.Close)
	fixture := &wave34AuthorityFixture{
		id: id, profile: id + "_profile", store: st, ctx: ctx, pool: pool,
		runner: &wave34AuthorityRunner{},
	}
	fixture.resetRows(t)
	fixture.seedProfile(t)
	fixture.seedProviderPolicy(t)
	fixture.seedDeployment(t)
	registry := agentdb.NewRunnerRegistry(agentdb.DryRunProvisionRunner{})
	registry.Register(fixture.runner)
	fixture.handler = agentDBSubrouterWithRegistry(
		st, registry, nil, wave34TestLiveAuthority(),
	)
	return fixture
}

func wave34TestLiveAuthority() *agentDBLiveAuthority {
	return newAgentDBLiveAuthority(config.AgentDBConfig{
		LiveProvisioningEnabled: true, AllowPublicIP: false,
		RequireBackupBeforeDrop: true,
		Providers: map[string]config.AgentDBProviderConfig{
			agentdb.ProviderAWSRDS: {
				Enabled: true, AllowedRegions: []string{"*"},
				AllowedAccounts: []string{"123456789012"},
				MaxTTLSeconds:   86400, MaxCostUSD: 100,
			},
		},
	})
}

func (f *wave34AuthorityFixture) resetRows(t *testing.T) {
	t.Helper()
	previous, previousErr := f.store.ProviderConfig(f.ctx, agentdb.ProviderAWSRDS)
	if previousErr != nil && !errors.Is(previousErr, agentdb.ErrNotFound) {
		t.Fatalf("read provider config: %v", previousErr)
	}
	_, _ = f.pool.Exec(f.ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", f.id,
	)
	_, _ = f.pool.Exec(f.ctx,
		"DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", f.profile,
	)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(f.ctx,
			"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", f.id,
		)
		_, _ = f.pool.Exec(f.ctx,
			"DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", f.profile,
		)
		restoreWave34ProviderConfig(f, previous, previousErr)
	})
}

func restoreWave34ProviderConfig(
	f *wave34AuthorityFixture,
	previous agentdb.ProviderConfig,
	previousErr error,
) {
	if previousErr == nil {
		_, _ = f.store.UpsertProviderConfig(f.ctx, agentdb.ProviderConfigRequest{
			Provider: previous.Provider, Enabled: previous.Enabled,
			Settings: previous.Settings,
		})
		return
	}
	_, _ = f.pool.Exec(f.ctx,
		"DELETE FROM sage.agent_db_provider_configs WHERE provider=$1",
		agentdb.ProviderAWSRDS,
	)
}

func (f *wave34AuthorityFixture) seedProfile(t *testing.T) {
	t.Helper()
	_, err := f.store.UpsertSizeProfile(f.ctx, agentdb.SizeProfile{
		ProfileID: f.profile, Provider: agentdb.ProviderAWSRDS,
		ProvisioningLevel: agentdb.LevelInstance, Name: f.profile,
		StorageGB: 20, MonthlyBudgetUSD: 75,
		ProviderParams: map[string]any{
			"region": "us-east-1", "account": "123456789012",
			"db_instance_class": "db.t4g.micro",
		},
	})
	if err != nil {
		t.Fatalf("seed size profile: %v", err)
	}
}

func (f *wave34AuthorityFixture) seedProviderPolicy(t *testing.T) {
	t.Helper()
	_, err := f.store.UpsertProviderConfig(f.ctx, agentdb.ProviderConfigRequest{
		Provider: agentdb.ProviderAWSRDS, Enabled: true,
		Settings: map[string]any{
			"allowed_regions":  []any{"us-east-1"},
			"allowed_accounts": []any{"123456789012"},
			"allow_public_ip":  false, "max_ttl_seconds": 86400,
			"max_estimated_cost_usd":     100,
			"live_provisioning_enabled":  true,
			"require_backup_before_drop": true,
			"execution_mode":             agentdb.LiveModeApproval,
		},
	})
	if err != nil {
		t.Fatalf("seed provider policy: %v", err)
	}
}

func (f *wave34AuthorityFixture) seedDeployment(t *testing.T) {
	t.Helper()
	_, err := f.store.Provision(f.ctx, agentdb.RegisterRequest{
		DeploymentID: f.id, TenantID: "wave34", AgentID: "authority-api",
		Provider: agentdb.ProviderAWSRDS, ProvisioningLevel: agentdb.LevelInstance,
		SizeProfileID: f.profile, LeaseSeconds: 3600, BudgetUSD: 25,
	})
	if err != nil {
		t.Fatalf("provision fixture: %v", err)
	}
	if _, err := f.store.PreflightProvision(f.ctx, f.id); err != nil {
		t.Fatalf("preflight fixture: %v", err)
	}
}

func (f *wave34AuthorityFixture) promoteToLive(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, `UPDATE sage.agent_db_deployments
		SET provisioning_status='available', live_mode=true,
			provider_resource_id='wave34-live-resource'
		WHERE deployment_id=$1`, f.id)
	if err != nil {
		t.Fatalf("promote fixture to live: %v", err)
	}
	_, err = f.store.RecordBackup(f.ctx, f.id, agentdb.BackupRequest{
		BackupID: f.id + "_restore_verified", Provider: agentdb.ProviderAWSRDS,
		Status: "restore_verified",
	})
	if err != nil {
		t.Fatalf("record verified restore: %v", err)
	}
}

func (f *wave34AuthorityFixture) authorizePath() string {
	return "/api/v1/agent-dbs/" + f.id + "/provision/authorize-live"
}

func issueWave34Authorization(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	operation string,
	idempotencyKey string,
) wave34AuthorizationResponse {
	t.Helper()
	return issueWave34TupleForHandler(
		t, fixture.handler, fixture.id, operation, idempotencyKey,
	)
}

func issueWave34TupleForHandler(
	t *testing.T,
	handler http.Handler,
	deploymentID string,
	operation string,
	idempotencyKey string,
) wave34AuthorizationResponse {
	t.Helper()
	path := "/api/v1/agent-dbs/" + deploymentID + "/provision/authorize-live"
	rr := wave34Post(t, handler, path, map[string]any{
		"operation": operation, "idempotency_key": idempotencyKey,
	}, testAdminUser())
	if rr.Code != http.StatusOK {
		t.Fatalf("issue %s authorization status = %d, want 200; body=%s",
			operation, rr.Code, rr.Body.String())
	}
	var response wave34AuthorizationResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("decode %s authorization: %v", operation, err)
	}
	return response
}

func assertWave34IssueDenied(
	t *testing.T,
	handler http.Handler,
	path string,
	body map[string]any,
	user *auth.User,
	want int,
) {
	t.Helper()
	rr := wave34Post(t, handler, path, body, user)
	if rr.Code != want {
		t.Fatalf("authorization role gate status = %d, want %d; body=%s",
			rr.Code, want, rr.Body.String())
	}
}

func assertWave34SameTuple(
	t *testing.T,
	first wave34AuthorizationResponse,
	second wave34AuthorizationResponse,
) {
	t.Helper()
	if first.PlanHash != second.PlanHash || first.EstimateID != second.EstimateID ||
		first.AuthorizationID != second.AuthorizationID ||
		first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("authorization replay changed tuple: first=%+v second=%+v", first, second)
	}
}

func assertWave34ServerSummary(
	t *testing.T,
	response wave34AuthorizationResponse,
	deploymentID string,
	operation agentdb.ProvisionOperation,
) {
	t.Helper()
	if response.PlanHash == "" || response.EstimateID == "" ||
		response.AuthorizationID == "" || response.IdempotencyKey == "" {
		t.Fatalf("issued tuple is incomplete: %+v", response)
	}
	if response.Plan.DeploymentID != deploymentID ||
		response.Plan.Operation != operation ||
		response.Plan.Hash != response.PlanHash ||
		response.Plan.Provider != agentdb.ProviderAWSRDS {
		t.Fatalf("server plan summary is not exact: %+v", response.Plan)
	}
	if response.Estimate.EstimateID != response.EstimateID ||
		response.Estimate.EstimatedCostUSD <= 0 ||
		response.Estimate.PricingRevision == "" || response.Estimate.Confidence == "" {
		t.Fatalf("server estimate summary is incomplete: %+v", response.Estimate)
	}
}

func assertWave34PersistedTuple(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	response wave34AuthorizationResponse,
	operation agentdb.ProvisionOperation,
) {
	t.Helper()
	var planHash, estimateID, requesterID, idempotencyKey string
	err := fixture.pool.QueryRow(fixture.ctx, `SELECT plan_hash, estimate_id,
		requester_id, idempotency_key
		FROM sage.agent_db_live_authorizations
		WHERE authorization_id=$1 AND deployment_id=$2 AND operation=$3`,
		response.AuthorizationID, fixture.id, operation,
	).Scan(&planHash, &estimateID, &requesterID, &idempotencyKey)
	if err != nil {
		t.Fatalf("load persisted authorization: %v", err)
	}
	if planHash != response.PlanHash || estimateID != response.EstimateID ||
		idempotencyKey != response.IdempotencyKey || requesterID == "" ||
		requesterID == "forged-body" {
		t.Fatalf("persisted tuple mismatch: plan=%q estimate=%q requester=%q key=%q",
			planHash, estimateID, requesterID, idempotencyKey)
	}
	var count int
	err = fixture.pool.QueryRow(fixture.ctx, `SELECT count(*)
		FROM sage.agent_db_live_authorizations
		WHERE deployment_id=$1 AND operation=$2 AND idempotency_key=$3`,
		fixture.id, operation, response.IdempotencyKey,
	).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("persisted authorization count = %d, err=%v, want 1", count, err)
	}
}

func assertWave34WrongActorRejected(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	tuple wave34AuthorizationResponse,
) {
	t.Helper()
	other := &auth.User{ID: 99, Email: "other-admin@test.com", Role: auth.RoleAdmin}
	rr := wave34Post(t, fixture.handler, fixture.executePath(), wave34TupleBody(tuple), other)
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusForbidden {
		t.Fatalf("wrong authenticated actor status = %d, want 400 or 403; body=%s",
			rr.Code, rr.Body.String())
	}
	if got := fixture.runner.createCalls.Load(); got != 0 {
		t.Fatalf("provider calls after wrong actor = %d, want 0", got)
	}
}

func assertWave34MismatchedTupleConflicts(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	tuple wave34AuthorizationResponse,
) {
	t.Helper()
	body := wave34TupleBody(tuple)
	body["estimate_id"] = "different-estimate"
	assertWave34ExecuteStatus(t, fixture, body, http.StatusConflict)
	if got := fixture.runner.createCalls.Load(); got != 0 {
		t.Fatalf("provider calls after tuple mismatch = %d, want 0", got)
	}
}

func (f *wave34AuthorityFixture) executePath() string {
	return "/api/v1/agent-dbs/" + f.id + "/provision/execute"
}

func assertWave34ExecuteStatus(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	body map[string]any,
	want int,
) *httptest.ResponseRecorder {
	t.Helper()
	rr := wave34Post(t, fixture.handler, fixture.executePath(), body, testAdminUser())
	if rr.Code != want {
		t.Fatalf("live execute status = %d, want %d; body=%s", rr.Code, want, rr.Body.String())
	}
	return rr
}

func assertWave34DestroyStatus(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	body map[string]any,
	want int,
) {
	t.Helper()
	path := "/api/v1/agent-dbs/" + fixture.id + "/provision/destroy-live"
	rr := wave34Post(t, fixture.handler, path, body, testAdminUser())
	if rr.Code != want {
		t.Fatalf("live destroy status = %d, want %d; body=%s", rr.Code, want,
			rr.Body.String())
	}
}

func assertWave34ReceiptPersisted(
	t *testing.T,
	fixture *wave34AuthorityFixture,
	tuple wave34AuthorizationResponse,
) {
	t.Helper()
	var idempotencyKey, planHash string
	err := fixture.pool.QueryRow(fixture.ctx, `SELECT idempotency_key, plan_hash
		FROM sage.agent_db_live_receipts WHERE authorization_id=$1`,
		tuple.AuthorizationID,
	).Scan(&idempotencyKey, &planHash)
	if err != nil {
		t.Fatalf("load persisted live receipt: %v", err)
	}
	if idempotencyKey != tuple.IdempotencyKey || planHash != tuple.PlanHash {
		t.Fatalf("persisted receipt mismatch: key=%q plan=%q", idempotencyKey, planHash)
	}
}

func wave34TupleBody(response wave34AuthorizationResponse) map[string]any {
	return map[string]any{
		"mode": "live", "plan_hash": response.PlanHash,
		"estimate_id":      response.EstimateID,
		"authorization_id": response.AuthorizationID,
		"idempotency_key":  response.IdempotencyKey,
	}
}

func wave34ClientClaims() map[string]any {
	return map[string]any{
		"approved": false, "estimated_cost_usd": 999999.0,
		"cost_estimate_id": "forged-estimate", "actor_id": "forged-body",
		"admin_override_reason": "forged override",
		"policy": map[string]any{
			"live_provisioning_enabled": false, "execution_mode": "manual",
		},
	}
}

func wave34Post(
	t *testing.T,
	handler http.Handler,
	path string,
	body map[string]any,
	user *auth.User,
) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		req = withUser(req, user)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
