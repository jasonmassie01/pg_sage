package agentdb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func agentDBTestDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
}

func requireAgentDB(t *testing.T) (*Store, context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, agentDBTestDSN())
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unavailable: %v", err)
	}
	st := NewStore(pool)
	if err := st.Ensure(ctx); err != nil {
		pool.Close()
		t.Fatalf("ensure schema: %v", err)
	}
	return st, ctx, pool
}

func TestAgentDBSchema_HasFleetScaleIndexes(t *testing.T) {
	joined := strings.Join(schemaStatements, "\n")
	required := []string{
		"idx_agent_db_deployments_lease_expiry",
		"idx_agent_db_pings_deployment_time",
		"idx_agent_db_cost_samples_deployment_time",
		"idx_agent_db_backups_deployment_time",
		"idx_agent_db_tuning_hints_deployment_order",
		"idx_agent_db_audit_deployment_time",
	}
	for _, idx := range required {
		if !strings.Contains(joined, idx) {
			t.Errorf("schemaStatements missing %s", idx)
		}
	}
}

func TestStoreRequestIdempotencyAndDecisions(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE tenant_id=$1", "tenant_agentdb_test")

	body := map[string]any{"tenant_id": "tenant_agentdb_test", "agent_id": "agent_store", "requested_isolation_type": "schema"}
	created, err := st.CreateRequest(ctx, RequestCreate{TenantID: "tenant_agentdb_test", AgentID: "agent_store", OwnerID: "owner", Purpose: "unit", IsolationType: "schema", IdempotencyKey: "idem-1", Body: body})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if created.PolicyDecision != "allow" || created.Status != "approved" {
		t.Fatalf("decision/status = %s/%s", created.PolicyDecision, created.Status)
	}

	again, err := st.CreateRequest(ctx, RequestCreate{TenantID: "tenant_agentdb_test", AgentID: "agent_store", OwnerID: "owner", Purpose: "unit", IsolationType: "schema", IdempotencyKey: "idem-1", Body: body})
	if err != nil {
		t.Fatalf("CreateRequest idempotent: %v", err)
	}
	if again.RequestID != created.RequestID {
		t.Fatalf("idempotent request id = %s, want %s", again.RequestID, created.RequestID)
	}

	_, err = st.CreateRequest(ctx, RequestCreate{TenantID: "tenant_agentdb_test", AgentID: "agent_store", IsolationType: "schema", IdempotencyKey: "idem-1", Body: map[string]any{"different": true}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v, want ErrConflict", err)
	}

	listed, err := st.ListRequests(ctx)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("expected at least one request")
	}
	denied, err := st.SetRequestDecision(ctx, created.RequestID, DecisionRequest{Decision: "denied", Reason: "test"})
	if err != nil {
		t.Fatalf("SetRequestDecision: %v", err)
	}
	if denied.Status != "denied" || denied.PolicyDecision != "deny" {
		t.Fatalf("denied status/decision = %s/%s", denied.Status, denied.PolicyDecision)
	}

	approvedDatabase, err := st.CreateRequest(ctx, RequestCreate{
		RequestID:      "req_database_agentdb_test",
		TenantID:       "tenant_agentdb_test",
		AgentID:        "agent_store",
		IsolationType:  "database",
		IdempotencyKey: "idem-database",
		BackupRequired: true,
		Body: map[string]any{
			"tenant_id": "tenant_agentdb_test",
			"agent_id":  "agent_store",
			"mode":      "database",
		},
	})
	if err != nil {
		t.Fatalf("CreateRequest database: %v", err)
	}
	if approvedDatabase.PolicyDecision != "allow" || approvedDatabase.Status != "approved" {
		t.Fatalf(
			"database decision/status = %s/%s",
			approvedDatabase.PolicyDecision,
			approvedDatabase.Status,
		)
	}
}

func TestProvisionApprovedRequestCreatesLinkedDeployment(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	requestID := "req_provision_link"
	deploymentID := "dep_from_req_link"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE request_id=$1", requestID)

	req, err := st.CreateRequest(ctx, RequestCreate{
		RequestID:      requestID,
		TenantID:       "tenant_agentdb_test",
		AgentID:        "agent_request",
		RunID:          "run_request",
		Purpose:        "unit request provisioning",
		IsolationType:  LevelInstance,
		Provider:       ProviderGCPCloudSQL,
		DatabaseName:   "agent_app",
		BudgetUSD:      0,
		BackupRequired: true,
		Body: map[string]any{
			"tenant_id":                "tenant_agentdb_test",
			"agent_id":                 "agent_request",
			"provider":                 ProviderGCPCloudSQL,
			"requested_isolation_type": LevelInstance,
		},
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if req.Status != "requested" {
		t.Fatalf("request status = %s", req.Status)
	}
	if _, err := st.SetRequestDecision(ctx, requestID, DecisionRequest{
		Decision: "approved",
		Reason:   "unit approved",
	}); err != nil {
		t.Fatalf("SetRequestDecision: %v", err)
	}

	dep, err := st.ProvisionApprovedRequest(ctx, requestID, RequestProvisionRequest{
		DeploymentID: deploymentID,
		LeaseSeconds: 3600,
		ProviderParams: map[string]any{
			"project":             "demo-project",
			"region":              "us-central1",
			"database_version":    "POSTGRES_16",
			"tier":                "db-f1-micro",
			"edition":             "ENTERPRISE",
			"storage_size":        20,
			"ipv4_enabled":        true,
			"authorized_networks": []any{},
		},
	})
	if err != nil {
		t.Fatalf("ProvisionApprovedRequest: %v", err)
	}
	if dep.DeploymentID != deploymentID || dep.Provider != ProviderGCPCloudSQL {
		t.Fatalf("deployment = %#v", dep)
	}
	if dep.Metadata["request_id"] != requestID {
		t.Fatalf("metadata = %#v", dep.Metadata)
	}
	params, ok := dep.Metadata["provider_params"].(map[string]any)
	if !ok || params["project"] != "demo-project" {
		t.Fatalf("provider params = %#v", dep.Metadata)
	}
	if dep.BudgetUSD != 0 || !dep.BackupRequired {
		t.Fatalf("budget/backup = %.2f/%v", dep.BudgetUSD, dep.BackupRequired)
	}
}

func TestStoreDeploymentLifecycleRecommendationsAndCost(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_store_test"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	dep, err := st.Register(ctx, RegisterRequest{
		DeploymentID:   id,
		TenantID:       "tenant_agentdb_test",
		AgentID:        "agent_store",
		RunID:          "run1",
		DatabaseName:   "postgres",
		IsolationType:  "schema",
		LeaseSeconds:   60,
		BudgetUSD:      1,
		BackupRequired: true,
		Metadata: map[string]any{
			"purpose":        "test",
			"workload_types": []any{"vector", "postgis", "jsonb"},
			"extensions":     []any{"pgvector", "postgis"},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if dep.Status != "active" || dep.LeaseExpiresAt == nil {
		t.Fatalf("unexpected deployment: status=%s lease=%v", dep.Status, dep.LeaseExpiresAt)
	}

	pinged, err := st.Ping(ctx, id, PingRequest{Metrics: map[string]any{"qps": 1}})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if pinged.LastPingAt == nil {
		t.Fatal("expected last_ping_at")
	}

	extended, err := st.ExtendLease(ctx, id, LeaseRequest{LeaseSeconds: 120, Reason: "long task"})
	if err != nil {
		t.Fatalf("ExtendLease: %v", err)
	}
	if !extended.LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("lease not extended: %v", extended.LeaseExpiresAt)
	}

	_, err = st.UpsertRecommendation(ctx, id, RecommendationCreate{
		RecommendationID: "rec_store_test",
		Kind:             "query_rewrite",
		Title:            "rewrite",
		Detail:           "detail",
		QueryFingerprint: "select * from t",
	})
	if err != nil {
		t.Fatalf("seed recommendation: %v", err)
	}
	recs, err := st.Recommendations(ctx, id)
	if err != nil {
		t.Fatalf("Recommendations: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "rec_store_test" {
		t.Fatalf("recommendations = %#v", recs)
	}
	if err := st.Feedback(ctx, id, "rec_store_test", FeedbackRequest{Decision: "applied", Comment: "ok"}); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if err := st.AddCostSample(ctx, id, CostSampleRequest{CostUSD: 1.25, Metric: "tokens", Value: 10, Unit: "count"}); err != nil {
		t.Fatalf("AddCostSample: %v", err)
	}
	cost, err := st.Cost(ctx, id)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if cost.TotalUSD < 1.24 || cost.BudgetState != "hard_limit" {
		t.Fatalf("cost summary = %#v", cost)
	}
	hints, err := st.TuningHints(ctx, id)
	if err != nil {
		t.Fatalf("TuningHints: %v", err)
	}
	if len(hints) < 4 {
		t.Fatalf("expected vector/postgis/jsonb/extension hints, got %#v", hints)
	}

	if err := st.Delete(ctx, id); !errors.Is(err, ErrDeleteBlocked) {
		t.Fatalf("delete active err = %v, want ErrDeleteBlocked", err)
	}
	archived, err := st.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if archived.Status != "archived" {
		t.Fatalf("archive status = %s", archived.Status)
	}
	if err := st.Delete(ctx, id); !errors.Is(err, ErrRestoreRequired) {
		t.Fatalf("delete before verified restore err = %v, want ErrRestoreRequired", err)
	}
	if _, err := st.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_store_test",
		Provider: "managed",
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	restored, err := st.Restore(ctx, id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Status != "active" {
		t.Fatalf("restore status = %s", restored.Status)
	}
	archived, err = st.Archive(ctx, id)
	if err != nil {
		t.Fatalf("Archive after restore: %v", err)
	}
	if archived.Status != "archived" {
		t.Fatalf("archive status after restore = %s", archived.Status)
	}
	if err := st.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = st.Get(ctx, "missing-agentdb")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
}

func TestRecommendationContractStoresActionMetadataAndFeedback(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_recommendation_contract"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "tenant_agentdb_test",
		AgentID:       "agent_contract",
		IsolationType: "schema",
		LeaseSeconds:  3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = st.UpsertRecommendation(ctx, id, RecommendationCreate{
		RecommendationID: "rec_contract",
		Kind:             "query_rewrite",
		Title:            "Add LIMIT",
		Detail:           "Bound the agent vector lookup.",
		QueryFingerprint: "fp-contract",
		ActionType:       "query_rewrite",
		ActionRisk:       "safe",
		Confidence:       0.82,
		AgentInstructions: map[string]any{
			"expected_change": "add LIMIT",
		},
		Payload: map[string]any{
			"sql_before": "select * from items",
			"sql_after":  "select * from items limit 10",
		},
	})
	if err != nil {
		t.Fatalf("UpsertRecommendation: %v", err)
	}

	recs, err := st.Recommendations(ctx, id)
	if err != nil {
		t.Fatalf("Recommendations: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("recommendation count = %d", len(recs))
	}
	rec := recs[0]
	if rec.ActionType != "query_rewrite" || rec.ActionRisk != "safe" {
		t.Fatalf("action metadata = %#v", rec)
	}
	if rec.Confidence < 0.81 || rec.Confidence > 0.83 {
		t.Fatalf("confidence = %f", rec.Confidence)
	}
	if rec.AgentInstructions["expected_change"] != "add LIMIT" {
		t.Fatalf("agent instructions = %#v", rec.AgentInstructions)
	}
	if rec.Payload["sql_after"] != "select * from items limit 10" {
		t.Fatalf("payload = %#v", rec.Payload)
	}

	err = st.Feedback(ctx, id, "rec_contract", FeedbackRequest{
		Decision: "accepted",
		Comment:  "agent applied rewrite",
		Applied:  true,
		Result:   "rewrote query",
	})
	if err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	recs, err = st.Recommendations(ctx, id)
	if err != nil {
		t.Fatalf("Recommendations after feedback: %v", err)
	}
	if recs[0].Status != "accepted" {
		t.Fatalf("status = %s", recs[0].Status)
	}
	if recs[0].Feedback["result"] != "rewrote query" ||
		recs[0].Feedback["applied"] != true {
		t.Fatalf("feedback = %#v", recs[0].Feedback)
	}
}

func TestAuditEventsListAndExportJSONL(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_audit_contract"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_audit WHERE deployment_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "tenant_agentdb_test",
		AgentID:       "agent_audit",
		IsolationType: "schema",
		LeaseSeconds:  3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := st.Ping(ctx, id, PingRequest{
		Status:  "active",
		Metrics: map[string]any{"qps": 2},
	}); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := st.UpsertRecommendation(ctx, id, RecommendationCreate{
		RecommendationID: "rec_audit",
		Kind:             "query_rewrite",
		Title:            "Bound query",
		ActionType:       "query_rewrite",
		ActionRisk:       "safe",
	}); err != nil {
		t.Fatalf("UpsertRecommendation: %v", err)
	}
	if err := st.Feedback(ctx, id, "rec_audit", FeedbackRequest{
		Decision: "accepted",
		Applied:  true,
		Result:   "agent rewrote SQL",
	}); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if _, err := st.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_audit",
		Provider: "managed",
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	events, err := st.AuditEvents(ctx, id)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	if len(events) < 4 {
		t.Fatalf("audit event count = %d, events=%#v", len(events), events)
	}
	if events[0].Event != "register" || events[0].DeploymentID != id {
		t.Fatalf("first event = %#v", events[0])
	}
	if !auditHasEvent(events, "recommendation_feedback") ||
		!auditHasEvent(events, "backup_restore_verified") {
		t.Fatalf("missing expected audit events: %#v", events)
	}

	jsonl, err := st.AuditJSONL(ctx, id)
	if err != nil {
		t.Fatalf("AuditJSONL: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(jsonl)), "\n")
	if len(lines) != len(events) {
		t.Fatalf("jsonl line count = %d, events=%d body=%s",
			len(lines), len(events), string(jsonl))
	}
	var decoded AuditEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &decoded); err != nil {
		t.Fatalf("decode final JSONL line: %v line=%q", err, lines[len(lines)-1])
	}
	if decoded.DeploymentID != id || decoded.Event == "" || decoded.CreatedAt.IsZero() {
		t.Fatalf("decoded audit event = %#v", decoded)
	}
}

func auditHasEvent(events []AuditEvent, event string) bool {
	for _, item := range events {
		if item.Event == event {
			return true
		}
	}
	return false
}

func TestProvisionSchemaCreatesSanitizedSchema(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_schema_test"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	dep, err := st.ProvisionSchema(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "Tenant With Spaces",
		AgentID:       "Agent#42",
		IsolationType: "schema",
		SchemaName:    "Bad Schema Name!!",
		LeaseSeconds:  60,
	})
	if err != nil {
		t.Fatalf("ProvisionSchema: %v", err)
	}
	if dep.SchemaName != "bad_schema_name" {
		t.Fatalf("schema name = %q, want sanitized bad_schema_name", dep.SchemaName)
	}

	var exists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.schemata
			WHERE schema_name = $1
		)`, dep.SchemaName).Scan(&exists)
	if err != nil {
		t.Fatalf("schema existence query: %v", err)
	}
	if !exists {
		t.Fatalf("schema %q was not created", dep.SchemaName)
	}
	if dep.Metadata["credential_scope"] == "" {
		t.Fatalf("expected credential_scope metadata, got %#v", dep.Metadata)
	}
	_, _ = pool.Exec(ctx, `DROP SCHEMA IF EXISTS bad_schema_name CASCADE`)
}

func TestProvisionDatabaseCreatesLocalDatabase(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_database_test"
	dbName := "agentdb_local_database_test_" + idFrom(time.Now().UTC().String())
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	_, _ = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(dbName))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(dbName))
	})

	dep, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_database",
		Provider:          ProviderLocalPostgres,
		ProvisioningLevel: LevelDatabase,
		DatabaseName:      dbName,
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision database: %v", err)
	}
	if dep.DatabaseName != dbName {
		t.Fatalf("database name = %q, want %q", dep.DatabaseName, dbName)
	}
	if dep.ProvisioningStatus != "provisioned" {
		t.Fatalf("provisioning status = %q", dep.ProvisioningStatus)
	}
	if dep.ConnectionInfo["database_name"] != dbName {
		t.Fatalf("connection info = %#v", dep.ConnectionInfo)
	}

	var exists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_database WHERE datname = $1
		)`, dbName).Scan(&exists)
	if err != nil {
		t.Fatalf("database existence query: %v", err)
	}
	if !exists {
		t.Fatalf("database %q was not created", dbName)
	}
}
