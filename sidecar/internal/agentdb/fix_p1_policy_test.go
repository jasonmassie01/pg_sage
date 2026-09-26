package agentdb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// G8-B08: an unknown instance class has no price, so live issuance refuses
// instead of pricing it at a flat $100/month.
func TestUnknownInstanceClassDeniedLive(t *testing.T) {
	input := wave3LiveIssueInput(time.Now().UTC(), ProviderAWSRDS, LiveModeApproval)
	params := input.Deployment.Metadata["provider_params"].(map[string]any)
	params["db_instance_class"] = "db.r6i.32xlarge"
	if _, err := IssueLiveExecutionRecords(input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown class issuance err = %v, want ErrInvalid", err)
	}
}

// G8-B08: a low-confidence estimate is compared at double its value.
func TestLowConfidenceEstimateIsDoubledAgainstCeiling(t *testing.T) {
	policy := LiveProvisionPolicy{LiveProvisioningEnabled: true, ProviderEnabled: true,
		AllowedRegions: []string{"*"}, MaxTTLSeconds: 86400, MaxEstimatedCostUSD: 10}
	decision := EvaluateLiveProvisionPolicy(policy, LiveProvisionRequest{
		Region: "us-east-1", TTLSeconds: 3600, EstimatedCostUSD: 6,
		EstimatedCostDoubled: true, Approved: true,
	})
	if decision.Allowed {
		t.Fatalf("doubled 6 USD estimate passed a 10 USD ceiling: %+v", decision)
	}
}

// G8-B08 / D04: BudgetGate compares against the deployment budget at issue.
func TestIssueLiveRecordsAppliesDeploymentBudgetGate(t *testing.T) {
	input := wave3LiveIssueInput(time.Now().UTC(), ProviderAWSRDS, LiveModeApproval)
	input.Deployment.BudgetUSD = 0.001
	if _, err := IssueLiveExecutionRecords(input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-budget issuance err = %v, want ErrInvalid", err)
	}
}

// G8-B08: extend-lease is capped by policy TTL and by the deployment budget.
func TestExtendLeaseCappedByPolicyTTLAndBudget(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_extend_cap"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET budget_usd=1 WHERE deployment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExtendLease(ctx, id, LeaseRequest{LeaseSeconds: 7200,
		MaxTTLSeconds: 3600}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("lease beyond policy TTL err = %v, want ErrInvalid", err)
	}
	year := 365 * 24 * 3600
	if _, err := st.ExtendLease(ctx, id, LeaseRequest{LeaseSeconds: year}); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("lease beyond budget err = %v, want ErrInvalid", err)
	}
	if _, err := st.ExtendLease(ctx, id, LeaseRequest{LeaseSeconds: 600,
		MaxTTLSeconds: 3600}); err != nil {
		t.Fatalf("in-policy extension: %v", err)
	}
}

func seedDeniedRequest(t *testing.T, st *Store, ctx context.Context, id string) Request {
	t.Helper()
	_, _ = st.pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE request_id=$1", id)
	req, err := st.CreateRequest(ctx, RequestCreate{RequestID: id,
		TenantID: "tenant_agentdb_test", AgentID: "agent_policy", IsolationType: "schema",
		DataClassification: "phi"})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if req.PolicyDecision != "deny" {
		t.Fatalf("fixture decision = %s, want deny", req.PolicyDecision)
	}
	return req
}

// G8-B10: an operator approve cannot flip a policy deny.
func TestOperatorApproveCannotOverridePolicyDeny(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedDeniedRequest(t, st, ctx, "req_fix_policy_deny")
	if _, err := st.SetRequestDecision(ctx, "req_fix_policy_deny",
		DecisionRequest{Decision: "approved"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve of policy deny err = %v, want ErrConflict", err)
	}
	got, err := st.GetRequest(ctx, "req_fix_policy_deny")
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicyDecision != "deny" || got.Status == "approved" {
		t.Fatalf("policy deny overridden: %s/%s", got.PolicyDecision, got.Status)
	}
}

// G8-B10: an approved request provisions at most one deployment.
func TestApprovedRequestIsSingleUse(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "req_fix_single_use"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE request_id=$1", id)
	for _, dep := range []string{"dep_fix_single_1", "dep_fix_single_2"} {
		cleanupDeployment(t, ctx, pool, dep)
	}
	if _, err := st.CreateRequest(ctx, RequestCreate{RequestID: id,
		TenantID: "tenant_agentdb_test", AgentID: "agent_single", IsolationType: "schema",
	}); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	st.opts.LocalProvisioning = true
	if _, err := st.ProvisionApprovedRequest(ctx, id,
		RequestProvisionRequest{DeploymentID: "dep_fix_single_1"}); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if _, err := st.ProvisionApprovedRequest(ctx, id,
		RequestProvisionRequest{DeploymentID: "dep_fix_single_2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second provision err = %v, want ErrConflict", err)
	}
	if _, err := st.Get(ctx, "dep_fix_single_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second deployment created from one approval: %v", err)
	}
}

// G8-B10: an approved blueprint's spec wins over provision-time overrides.
func TestApprovedBlueprintSpecWinsOverOverrides(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	bpID, depID := "bp_fix_spec_wins", "dep_fix_spec_wins"
	cleanupDeployment(t, ctx, pool, depID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", bpID)
	bp, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{BlueprintID: bpID,
		Name: "spec wins", CreatedBy: "unit"}, staticBlueprintGenerator{spec: BlueprintSpec{
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, Region: "us-east-2",
		InstanceClass: "db.t4g.micro", StorageGB: 20, BackupRetentionDays: 1,
		PrivateNetwork: true,
	}})
	if err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if _, err := st.ApproveBlueprint(ctx, bp.BlueprintID, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	dep, err := st.ProvisionFromBlueprint(ctx, bpID, BlueprintProvisionRequest{
		DeploymentID: depID, TenantID: "tenant_agentdb_test", AgentID: "agent_bp",
		LeaseSeconds: 3600, ProviderParams: map[string]any{
			"region": "eu-west-1", "db_instance_class": "db.r6i.32xlarge"},
	})
	if err != nil {
		t.Fatalf("ProvisionFromBlueprint: %v", err)
	}
	params := dep.Metadata["provider_params"].(map[string]any)
	if params["region"] != "us-east-2" || params["db_instance_class"] != "db.t4g.micro" {
		t.Fatalf("override replaced approved spec: %#v", params)
	}
}

// G8-B11: restore_verified cannot be self-attested through the generic
// backup recorder, and a backup id cannot be re-pointed at another deployment.
func TestRecordBackupRestoreVerifiedRequiresDrillAndScope(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	a, b := "adb_fix_backup_a", "adb_fix_backup_b"
	seedExpiredLiveDeployment(t, st, ctx, pool, a)
	seedExpiredLiveDeployment(t, st, ctx, pool, b)
	if _, err := st.RecordBackup(ctx, a, BackupRequest{BackupID: "bk_fix_self",
		Status: "restore_verified"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self-attested restore_verified err = %v, want ErrInvalid", err)
	}
	if _, err := st.RecordBackup(ctx, b, BackupRequest{BackupID: "bk_fix_owned_by_b",
		Status: "ready"}); err != nil {
		t.Fatalf("record b backup: %v", err)
	}
	if _, err := st.RecordBackup(ctx, a, BackupRequest{BackupID: "bk_fix_owned_by_b",
		Status: "verified"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-deployment backup upsert err = %v, want ErrConflict", err)
	}
	backups, err := st.Backups(ctx, b)
	if err != nil || len(backups) != 1 || backups[0].Status != "ready" {
		t.Fatalf("deployment b backup tampered: %#v %v", backups, err)
	}
}

// SURF-06: a dry-run backup check never records "verified" evidence.
func TestDryRunBackupCheckDoesNotRecordVerified(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_dry_backup"
	cleanupDeployment(t, ctx, pool, id)
	if _, err := st.Provision(ctx, RegisterRequest{DeploymentID: id,
		TenantID: "tenant_agentdb_test", AgentID: "agent_backup", Provider: ProviderAWSRDS,
		ProvisioningLevel: LevelInstance, LeaseSeconds: 3600}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	assurance, err := st.CheckBackupAssurance(ctx, id, DryRunProvisionRunner{})
	if err != nil {
		t.Fatalf("CheckBackupAssurance: %v", err)
	}
	if assurance.BackupStatus == "verified" || assurance.Backup.Status == "verified" {
		t.Fatalf("dry-run check recorded verified evidence: %#v", assurance.Backup)
	}
}

// SURF-05: template provisioning is labelled review-only and bound to the
// approved content hash.
func TestTerraformTemplateProvisionIsReviewOnlyAndHashBound(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	tplID, depID := "tf_fix_review_only", "dep_fix_tf_review"
	cleanupDeployment(t, ctx, pool, depID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1",
		tplID)
	tpl, err := st.CreateTerraformTemplate(ctx, TerraformTemplateRequest{TemplateID: tplID,
		Name: "review", CreatedBy: "unit",
		Files: []TerraformFile{{Path: "main.tf", Body: `resource "aws_db_instance" "db" {}`}}})
	if err != nil {
		t.Fatalf("CreateTerraformTemplate: %v", err)
	}
	if _, err := st.ApproveTerraformTemplate(ctx, tplID, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	dep, err := st.ProvisionFromTerraformTemplate(ctx, tplID, TemplateProvisionRequest{
		DeploymentID: depID, TenantID: "tenant_agentdb_test", AgentID: "agent_tf",
		Provider: ProviderAWSRDS, LeaseSeconds: 3600,
		ProviderParams: map[string]any{"region": "us-east-1"},
	})
	if err != nil {
		t.Fatalf("ProvisionFromTerraformTemplate: %v", err)
	}
	if dep.ProvisioningPlan["template_semantics"] != "review_only" ||
		dep.Metadata["terraform_template_sha256"] != tpl.ContentSHA256 {
		t.Fatalf("template provision not labelled review-only/hash-bound: plan=%#v meta=%#v",
			dep.ProvisioningPlan, dep.Metadata)
	}
}

// G8-B29: negative cost samples are rejected.
func TestNegativeCostSampleRejected(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_negative_cost"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if err := st.AddCostSample(ctx, id, CostSampleRequest{CostUSD: -5}); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("negative cost err = %v, want ErrInvalid", err)
	}
}

// G8-B28: HCL template expressions in LLM-derived fields are neutralised.
func TestRenderedTerraformEscapesTemplateExpressions(t *testing.T) {
	out := renderAWSRDS(BlueprintSpec{Region: `${file("/etc/passwd")}`,
		InstanceClass: `%{ if true }x%{ endif }`, DatabaseVersion: "16",
		Extensions: []string{"${var.x}"}})
	if strings.Contains(out, `"${file`) || strings.Contains(out, `"%{ if`) ||
		strings.Contains(out, `"${var`) {
		t.Fatalf("unescaped HCL template expression in render:\n%s", out)
	}
}

// G8-B16: once the schema is initialized in-process, Ensure is memoized and
// does not wait on the database-wide initialization lock again.
func TestEnsureIsMemoizedPerPool(t *testing.T) {
	pool := freshEnsurePool(t)
	if err := NewStore(pool).Ensure(t.Context()); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock($1)",
		int64(0x5047534741474442)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	for range 20 {
		if err := NewStore(pool).Ensure(ctx); err != nil {
			t.Fatalf("memoized Ensure re-ran initialization: %v", err)
		}
	}
}

// G8-B17: local_postgres DDL is disabled unless explicitly enabled.
func TestLocalProvisioningDisabledByDefault(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_local_disabled"
	cleanupDeployment(t, ctx, pool, id)
	_, err := NewStore(pool).Provision(ctx, RegisterRequest{DeploymentID: id,
		TenantID: "tenant_agentdb_test", AgentID: "agent_local", Provider: ProviderLocalPostgres,
		ProvisioningLevel: LevelSchema, SchemaName: "agentdb_fix_local_disabled"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("local provisioning without opt-in err = %v, want ErrInvalid", err)
	}
	_ = st
}

// G8-B12: require_backup_before_destroy=false is honoured at registration.
func TestRegisterHonoursBackupPolicyOption(t *testing.T) {
	_, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_backup_optional_register"
	cleanupDeployment(t, ctx, pool, id)
	relaxed := NewStoreWithOptions(pool, StoreOptions{RequireBackupBeforeDestroy: false})
	dep, err := relaxed.Register(ctx, RegisterRequest{DeploymentID: id,
		TenantID: "tenant_agentdb_test", AgentID: "agent_backup_opt",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if dep.BackupRequired {
		t.Fatal("backup_required forced true although policy disables it")
	}
}

// G8-B17: local provisioning never adopts an existing schema.
func TestLocalProvisioningRefusesToAdoptExistingSchema(t *testing.T) {
	_, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_local_adopt"
	cleanupDeployment(t, ctx, pool, id)
	local := NewStoreWithOptions(pool, StoreOptions{
		RequireBackupBeforeDestroy: true, LocalProvisioning: true})
	_, err := local.Provision(ctx, RegisterRequest{DeploymentID: id,
		TenantID: "tenant_agentdb_test", AgentID: "agent_local", Provider: ProviderLocalPostgres,
		ProvisioningLevel: LevelSchema, SchemaName: "sage"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("adopting existing schema err = %v, want ErrConflict", err)
	}
	if _, err := local.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deployment registered over an adopted schema: %v", err)
	}
}
