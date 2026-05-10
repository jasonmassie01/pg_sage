package agentdb

import (
	"context"
	"testing"
	"time"
)

func TestLiveExecuteAndDestroyUseProviderRunner(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_live_exec_provider"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_live",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	runner := fakeProviderRunner{provider: ProviderGCPCloudSQL, name: "fake_cloudsql"}
	attempt, err := st.ExecuteProvisionLive(ctx, id, runner, LiveExecutionRequest{
		CostEstimateID: "estimate-1",
	})
	if err != nil {
		t.Fatalf("ExecuteProvisionLive: %v", err)
	}
	if attempt.Kind != "execute_live" || attempt.Runner != "fake_cloudsql" {
		t.Fatalf("attempt = %#v", attempt)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.ProvisioningStatus != "available" ||
		dep.ProviderResourceID != "live-resource" ||
		!dep.LiveMode {
		t.Fatalf("deployment after live execute = %#v", dep)
	}
	if _, err := st.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_live_exec",
		Provider: ProviderGCPCloudSQL,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	destroy, err := st.DestroyProvisionLive(ctx, id, runner)
	if err != nil {
		t.Fatalf("DestroyProvisionLive: %v", err)
	}
	if destroy.Kind != "destroy_live" {
		t.Fatalf("destroy attempt = %#v", destroy)
	}
}

func TestLiveExecuteCanPromoteAfterDryRun(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_live_after_dry_run"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_live",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	if _, err := st.ExecuteProvision(ctx, id, &recordingProvisionRunner{}); err != nil {
		t.Fatalf("ExecuteProvision dry-run: %v", err)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after dry-run: %v", err)
	}
	if dep.ProvisioningStatus != "dry_run_ready" || dep.LiveMode {
		t.Fatalf("deployment after dry-run = %#v", dep)
	}

	attempt, err := st.ExecuteProvisionLive(
		ctx,
		id,
		fakeProviderRunner{provider: ProviderAWSRDS, name: "fake_rds"},
		LiveExecutionRequest{CostEstimateID: "estimate-after-dry-run"},
	)
	if err != nil {
		t.Fatalf("ExecuteProvisionLive after dry-run: %v", err)
	}
	if attempt.Kind != "execute_live" || attempt.Status != "succeeded" {
		t.Fatalf("live attempt = %#v", attempt)
	}
	dep, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after live execute: %v", err)
	}
	if dep.ProvisioningStatus != "available" || !dep.LiveMode ||
		dep.ProviderResourceID != "live-resource" {
		t.Fatalf("deployment after live execute = %#v", dep)
	}
}

func TestReconcileLiveProvisioning(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_reconcile_live"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:       id,
		TenantID:           "tenant_agentdb_test",
		AgentID:            "agent_reconcile_live",
		Provider:           ProviderAWSRDS,
		ProvisioningLevel:  LevelInstance,
		ProvisioningStatus: "planned",
		LeaseSeconds:       3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provisioning_status='provisioning', provider_resource_id='live-resource'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	registry.Register(fakeProviderRunner{provider: ProviderAWSRDS, name: "fake_rds"})
	result, err := st.ReconcileLiveProvisioning(ctx, registry)
	if err != nil {
		t.Fatalf("ReconcileLiveProvisioning: %v", err)
	}
	if len(result.DestroyDryRun) == 0 {
		t.Fatalf("expected reconcile attempt: %#v", result)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.ProvisioningStatus != "available" {
		t.Fatalf("status = %s", dep.ProvisioningStatus)
	}
}

func TestReconcileAbandonedDeploymentsDestroysLiveCloudResources(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_reconcile_live_destroy"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:       id,
		TenantID:           "tenant_agentdb_test",
		AgentID:            "agent_reconcile_live",
		Provider:           ProviderAWSRDS,
		ProvisioningLevel:  LevelInstance,
		ProvisioningStatus: "available",
		LeaseSeconds:       60,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET lease_expires_at=now()-interval '2 hours',
			provisioning_status='available',
			live_mode=true,
			provider_resource_id='live-resource'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("expire live deployment: %v", err)
	}
	if _, err := st.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_reconcile_live_destroy",
		Provider: ProviderAWSRDS,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	registry.Register(fakeProviderRunner{provider: ProviderAWSRDS, name: "fake_rds"})

	result, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(), registry)
	if err != nil {
		t.Fatalf("ReconcileAbandonedDeployments: %v", err)
	}
	if !containsAttempt(result.DestroyLive, id, "destroy_live") {
		t.Fatalf("destroy live attempts = %#v", result.DestroyLive)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.ProvisioningStatus != "destroying" {
		t.Fatalf("status = %s, want destroying", dep.ProvisioningStatus)
	}
}

func TestLiveStatusAndBackupUseProviderRunner(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_live_status_provider"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_live_status",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provisioning_status='available', live_mode=true,
			provider_resource_id='live-resource'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("seed live status: %v", err)
	}
	runner := fakeProviderRunner{provider: ProviderAWSRDS, name: "fake_rds"}
	status, err := st.CheckProvisionStatusLive(ctx, id, runner)
	if err != nil {
		t.Fatalf("CheckProvisionStatusLive: %v", err)
	}
	if status.Runner != "fake_rds" || status.Kind != "status_check_live" {
		t.Fatalf("status attempt = %#v", status)
	}
	backup, err := st.CheckBackupAssuranceLive(ctx, id, runner)
	if err != nil {
		t.Fatalf("CheckBackupAssuranceLive: %v", err)
	}
	if backup.Attempt.Runner != "fake_rds" || backup.SafeForDestroy {
		t.Fatalf("backup assurance = %#v", backup)
	}
	if backup.BackupStatus == "restore_verified" {
		t.Fatalf("backup check must not mint restore verification: %#v", backup)
	}
}

func TestLiveBackupCheckDoesNotUnlockDestroy(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_live_backup_no_destroy_unlock"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:       id,
		TenantID:           "tenant_agentdb_test",
		AgentID:            "agent_live_status",
		Provider:           ProviderAWSRDS,
		ProvisioningLevel:  LevelInstance,
		ProvisioningStatus: "available",
		BackupRequired:     true,
		LeaseSeconds:       3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	runner := fakeProviderRunner{provider: ProviderAWSRDS, name: "fake_rds"}
	if _, err := st.CheckBackupAssuranceLive(ctx, id, runner); err != nil {
		t.Fatalf("CheckBackupAssuranceLive: %v", err)
	}
	if _, err := st.DestroyProvisionLive(ctx, id, runner); err == nil {
		t.Fatal("DestroyProvisionLive succeeded without a restore-verified backup")
	}
}

func TestLiveStatusDoesNotMarkDryRunDeploymentLive(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_status_preserves_dry_run"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_live_status",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provisioning_status='status_checked', live_mode=false,
			provider_resource_id='planned-resource'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("seed dry-run status: %v", err)
	}
	if _, err := st.CheckProvisionStatusLive(ctx, id,
		fakeProviderRunner{provider: ProviderAWSRDS, name: "fake_rds"}); err != nil {
		t.Fatalf("CheckProvisionStatusLive: %v", err)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.LiveMode {
		t.Fatalf("status check incorrectly flipped live_mode: %#v", dep)
	}
}

func TestLiveStatusMarksDestroyingDeploymentDestroyedWhenProviderMissing(
	t *testing.T,
) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_live_destroyed_after_missing"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_live_status",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provisioning_status='destroying', live_mode=true,
			provider_resource_id='live-resource'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("seed destroying status: %v", err)
	}
	attempt, err := st.CheckProvisionStatusLive(ctx, id,
		notFoundStatusRunner{fakeProviderRunner{
			provider: ProviderGCPCloudSQL,
			name:     "fake_cloudsql",
		}})
	if err != nil {
		t.Fatalf("CheckProvisionStatusLive: %v", err)
	}
	if attempt.Status != "succeeded" {
		t.Fatalf("attempt status = %s, want succeeded", attempt.Status)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.ProvisioningStatus != "destroyed" {
		t.Fatalf("status = %s, want destroyed", dep.ProvisioningStatus)
	}
}

type fakeProviderRunner struct {
	provider string
	name     string
}

func (f fakeProviderRunner) Name() string { return f.name }

func (f fakeProviderRunner) Provider() string { return f.provider }

func (f fakeProviderRunner) Preflight(context.Context, ProvisionRequest) ProvisionResult {
	return ProvisionResult{Status: "preflight_passed"}
}

func (f fakeProviderRunner) Create(context.Context, ProvisionRequest) ProvisionResult {
	return ProvisionResult{
		Status:             "available",
		ProviderResourceID: "live-resource",
		SecretRefProvider:  "fake_secret_manager",
		ConnectionInfo:     map[string]any{"endpoint": "host"},
		Detail:             map[string]any{"created_at": time.Now().UTC().String()},
	}
}

func (f fakeProviderRunner) Status(context.Context, ProvisionRequest) ProvisionResult {
	return ProvisionResult{
		Status:             "available",
		ProviderResourceID: "live-resource",
		ConnectionInfo:     map[string]any{"endpoint": "host"},
		Detail:             map[string]any{"state": "available"},
	}
}

func (f fakeProviderRunner) Destroy(context.Context, ProvisionRequest) ProvisionResult {
	return ProvisionResult{Status: "destroying", ProviderResourceID: "live-resource"}
}

func (f fakeProviderRunner) BackupCheck(context.Context, ProvisionRequest) ProvisionResult {
	return ProvisionResult{Status: "verified"}
}

type notFoundStatusRunner struct {
	fakeProviderRunner
}

func (r notFoundStatusRunner) Status(
	context.Context,
	ProvisionRequest,
) ProvisionResult {
	return ProvisionResult{
		Status: "status_unknown",
		Error: providerError(
			r.Provider(), ProviderErrNotFound, "resource not found", ""),
	}
}
