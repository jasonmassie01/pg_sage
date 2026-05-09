package agentdb

import (
	"errors"
	"testing"
)

func TestDeployRequestReviewLifecycle(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_deploy_request_contract"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "tenant_agentdb_test",
		AgentID:       "agent_deploy",
		RunID:         "run_deploy",
		IsolationType: "schema",
		LeaseSeconds:  3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	draft, err := st.CreateDeployRequest(ctx, id, DeployRequestCreate{
		DeployRequestID:    "dr_contract",
		Title:              "Promote useful schema",
		Reason:             "agent workspace is ready",
		TargetDatabaseName: "prod",
		TargetSchemaName:   "public",
		RiskTier:           "moderate",
		CreatedBy:          "operator",
		GateResults: map[string]any{
			"schema_lint": "pass",
		},
	})
	if err != nil {
		t.Fatalf("CreateDeployRequest draft: %v", err)
	}
	if draft.TenantID != "tenant_agentdb_test" ||
		draft.AgentID != "agent_deploy" ||
		draft.Status != "draft" {
		t.Fatalf("draft metadata = %#v", draft)
	}

	_, err = st.RequestDeployReview(ctx, id, "dr_contract")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("review without SQL err = %v, want ErrInvalid", err)
	}

	ready, err := st.CreateDeployRequest(ctx, id, DeployRequestCreate{
		DeployRequestID: "dr_ready",
		Title:           "Promote stable table",
		Reason:          "query validation passed",
		RiskTier:        "moderate",
		MigrationSQL:    "CREATE TABLE prod.agent_items(id bigint primary key);",
		VerificationSQL: "SELECT count(*) FROM prod.agent_items;",
		RollbackSQL:     "DROP TABLE prod.agent_items;",
		Status:          "review_requested",
		CreatedBy:       "operator",
	})
	if err != nil {
		t.Fatalf("CreateDeployRequest review_requested: %v", err)
	}
	if ready.Status != "review_requested" {
		t.Fatalf("ready status = %s", ready.Status)
	}
	if ready.GateResults["review_only"] != true {
		t.Fatalf("gate results = %#v", ready.GateResults)
	}

	listed, err := st.DeployRequests(ctx, id)
	if err != nil {
		t.Fatalf("DeployRequests: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("deploy request count = %d", len(listed))
	}

	if _, err := st.ReviewDeployRequest(ctx, id, "dr_ready", DeployRequestReview{
		Decision:   "approved",
		ReviewedBy: "dba",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approve without reason err = %v, want ErrInvalid", err)
	}
	approved, err := st.ReviewDeployRequest(ctx, id, "dr_ready", DeployRequestReview{
		Decision:     "approved",
		ReviewedBy:   "dba",
		ReviewReason: "migration reviewed",
	})
	if err != nil {
		t.Fatalf("ReviewDeployRequest approve: %v", err)
	}
	if approved.Status != "approved" || approved.ReviewedBy != "dba" {
		t.Fatalf("approved request = %#v", approved)
	}

	events, err := st.AuditEvents(ctx, id)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	if !auditHasEvent(events, "deploy_request_created") ||
		!auditHasEvent(events, "deploy_request_approved") {
		t.Fatalf("missing deploy request audit events: %#v", events)
	}
}

func TestDeployRequestRejectsInvalidDeploymentAndScope(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	for _, id := range []string{"adb_deploy_scope_a", "adb_deploy_scope_b"} {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
		_, err := st.Register(ctx, RegisterRequest{
			DeploymentID:  id,
			TenantID:      "tenant_agentdb_test",
			AgentID:       "agent_deploy",
			IsolationType: "schema",
			LeaseSeconds:  3600,
		})
		if err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}

	_, err := st.CreateDeployRequest(ctx, "missing_deploy", DeployRequestCreate{
		Title: "bad",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment err = %v, want ErrNotFound", err)
	}

	_, err = st.CreateDeployRequest(ctx, "adb_deploy_scope_a", DeployRequestCreate{
		DeployRequestID: "dr_scope",
		Title:           "Scoped request",
		MigrationSQL:    "CREATE TABLE x(id int);",
		VerificationSQL: "SELECT 1;",
		Status:          "review_requested",
	})
	if err != nil {
		t.Fatalf("CreateDeployRequest: %v", err)
	}
	if _, err := st.GetDeployRequest(ctx, "adb_deploy_scope_b", "dr_scope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-deployment get err = %v, want ErrNotFound", err)
	}
	if _, err := st.ReviewDeployRequest(ctx, "adb_deploy_scope_b", "dr_scope",
		DeployRequestReview{
			Decision:     "denied",
			ReviewedBy:   "dba",
			ReviewReason: "wrong deployment",
		}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-deployment review err = %v, want ErrNotFound", err)
	}
}
