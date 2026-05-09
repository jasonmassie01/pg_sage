package agentdb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPreflightAndExecuteCloudPlanDryRun(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_exec_dry_run"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	dep, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_exec",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}
	if dep.ProvisioningStatus != "planned" {
		t.Fatalf("initial status = %s", dep.ProvisioningStatus)
	}

	preflight, err := st.PreflightProvision(ctx, id)
	if err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	if preflight.Kind != "preflight" || preflight.Status != "passed" {
		t.Fatalf("preflight attempt = %#v", preflight)
	}
	dep, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after preflight: %v", err)
	}
	if dep.ProvisioningStatus != "preflight_passed" {
		t.Fatalf("status after preflight = %s", dep.ProvisioningStatus)
	}

	runner := &recordingProvisionRunner{}
	execAttempt, err := st.ExecuteProvision(ctx, id, runner)
	if err != nil {
		t.Fatalf("ExecuteProvision: %v", err)
	}
	if execAttempt.Kind != "execute" || execAttempt.Status != "succeeded" {
		t.Fatalf("execute attempt = %#v", execAttempt)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "aws rds") {
		t.Fatalf("runner commands = %#v", runner.commands)
	}
	dep, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after execute: %v", err)
	}
	if dep.ProvisioningStatus != "dry_run_ready" {
		t.Fatalf("status after execute = %s", dep.ProvisioningStatus)
	}
	if dep.ConnectionInfo["execution_mode"] != "dry_run" {
		t.Fatalf("connection info = %#v", dep.ConnectionInfo)
	}

	attempts, err := st.ProvisionAttempts(ctx, id)
	if err != nil {
		t.Fatalf("ProvisionAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempt count = %d, want 2", len(attempts))
	}
}

func TestExecuteProvisionRejectsUnsafeStates(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()

	localID := "adb_exec_local_reject"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", localID)
	_, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      localID,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_exec",
		Provider:          ProviderLocalPostgres,
		ProvisioningLevel: LevelSchema,
		SchemaName:        "adb_exec_local_reject",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision local schema: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, localID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("local preflight err = %v, want ErrInvalid", err)
	}

	cloudID := "adb_exec_needs_preflight"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", cloudID)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      cloudID,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_exec",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	}); err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}
	if _, err := st.ExecuteProvision(
		ctx, cloudID, &recordingProvisionRunner{},
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("execute before preflight err = %v, want ErrInvalid", err)
	}
	if _, err := st.PreflightProvision(ctx, cloudID); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	if _, err := st.ExecuteProvision(ctx, cloudID, failingProvisionRunner{}); err == nil {
		t.Fatal("expected runner failure")
	}
	if _, err := st.ExecuteProvision(ctx, cloudID, &recordingProvisionRunner{}); err != nil {
		t.Fatalf("execute after failed attempt: %v", err)
	}
	if _, err := st.ExecuteProvision(
		ctx, cloudID, &recordingProvisionRunner{},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate execute err = %v, want ErrConflict", err)
	}
}

func TestProvisionLifecycleStatusAndDestroyDryRun(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_lifecycle_cloudsql"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	_, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_lifecycle",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}

	statusAttempt, err := st.CheckProvisionStatus(ctx, id, &recordingProvisionRunner{})
	if err != nil {
		t.Fatalf("CheckProvisionStatus: %v", err)
	}
	if statusAttempt.Kind != "status_check" || statusAttempt.Status != "succeeded" {
		t.Fatalf("status attempt = %#v", statusAttempt)
	}
	if !strings.Contains(strings.Join(statusAttempt.Command, " "), "gcloud sql instances describe") {
		t.Fatalf("status command = %#v", statusAttempt.Command)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after status check: %v", err)
	}
	if dep.ProvisioningStatus != "status_checked" {
		t.Fatalf("status after check = %s", dep.ProvisioningStatus)
	}

	if _, err := st.DestroyProvisionDryRun(
		ctx, id, &recordingProvisionRunner{},
	); !errors.Is(err, ErrRestoreRequired) {
		t.Fatalf("destroy without restore err = %v, want ErrRestoreRequired", err)
	}

	if _, err := st.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_lifecycle_cloudsql",
		Provider: ProviderGCPCloudSQL,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	destroyAttempt, err := st.DestroyProvisionDryRun(
		ctx, id, &recordingProvisionRunner{},
	)
	if err != nil {
		t.Fatalf("DestroyProvisionDryRun: %v", err)
	}
	if destroyAttempt.Kind != "destroy_dry_run" || destroyAttempt.Status != "succeeded" {
		t.Fatalf("destroy attempt = %#v", destroyAttempt)
	}
	if !strings.Contains(strings.Join(destroyAttempt.Command, " "), "gcloud sql instances delete") {
		t.Fatalf("destroy command = %#v", destroyAttempt.Command)
	}
	dep, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after destroy dry-run: %v", err)
	}
	if dep.ProvisioningStatus != "destroy_dry_run_ready" {
		t.Fatalf("status after destroy dry-run = %s", dep.ProvisioningStatus)
	}
}

func TestReconcileAbandonedDeploymentsPlansSafeCloudDestroy(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	safeID := "adb_reconcile_lakebase_safe"
	blockedID := "adb_reconcile_rds_blocked"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=ANY($1)",
		[]string{safeID, blockedID},
	)

	for _, req := range []RegisterRequest{
		{
			DeploymentID:      safeID,
			TenantID:          "tenant_agentdb_test",
			AgentID:           "agent_reconcile",
			Provider:          ProviderDatabricksLakebase,
			ProvisioningLevel: LevelInstance,
			DatabaseName:      "safe_app",
			LeaseSeconds:      60,
		},
		{
			DeploymentID:      blockedID,
			TenantID:          "tenant_agentdb_test",
			AgentID:           "agent_reconcile",
			Provider:          ProviderAWSRDS,
			ProvisioningLevel: LevelInstance,
			DatabaseName:      "blocked_app",
			LeaseSeconds:      60,
		},
	} {
		if _, err := st.Provision(ctx, req); err != nil {
			t.Fatalf("Provision %s: %v", req.DeploymentID, err)
		}
	}
	_, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET lease_expires_at=now()-interval '2 hours'
		WHERE deployment_id=ANY($1)`,
		[]string{safeID, blockedID},
	)
	if err != nil {
		t.Fatalf("expire deployments: %v", err)
	}
	if _, err := st.RecordBackup(ctx, safeID, BackupRequest{
		BackupID: "backup_reconcile_lakebase",
		Provider: ProviderDatabricksLakebase,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	result, err := st.ReconcileAbandonedDeployments(
		ctx, time.Now().UTC(), &recordingProvisionRunner{},
	)
	if err != nil {
		t.Fatalf("ReconcileAbandonedDeployments: %v", err)
	}
	if !containsDeployment(result.Archived, safeID) ||
		!containsDeployment(result.Archived, blockedID) {
		t.Fatalf("archived missing seeded deployments: %#v", result.Archived)
	}
	if !containsAttempt(result.DestroyDryRun, safeID, "destroy_dry_run") {
		t.Fatalf("destroy dry-runs = %#v", result.DestroyDryRun)
	}
	if !containsBlocked(result.Blocked, blockedID, "verified restore required") {
		t.Fatalf("blocked = %#v", result.Blocked)
	}
}

func TestBackupAssuranceManagedProviderRecordsVerifiedCheck(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_backup_assurance_rds"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	_, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_backup",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      3600,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}

	assurance, err := st.CheckBackupAssurance(ctx, id, &recordingProvisionRunner{})
	if err != nil {
		t.Fatalf("CheckBackupAssurance: %v", err)
	}
	if assurance.Mode != "managed_provider" {
		t.Fatalf("mode = %s", assurance.Mode)
	}
	if assurance.SafeForDestroy {
		t.Fatalf("safe for destroy should require restore verification: %#v", assurance)
	}
	if assurance.BackupStatus != "verified" {
		t.Fatalf("backup status = %s", assurance.BackupStatus)
	}
	if assurance.Attempt.Kind != "backup_check" ||
		assurance.Attempt.Status != "succeeded" {
		t.Fatalf("attempt = %#v", assurance.Attempt)
	}
	if !strings.Contains(strings.Join(assurance.Attempt.Command, " "), "aws rds describe-db-instances") {
		t.Fatalf("backup command = %#v", assurance.Attempt.Command)
	}
	backups, err := st.Backups(ctx, id)
	if err != nil {
		t.Fatalf("Backups: %v", err)
	}
	if !containsBackupStatus(backups, "verified") {
		t.Fatalf("backups missing verified record: %#v", backups)
	}
}

func TestRestoreDrillDryRunDoesNotBypassVerifiedRestoreGate(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_restore_drill_local"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	_, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_backup",
		Provider:          ProviderLocalPostgres,
		ProvisioningLevel: LevelSchema,
		SchemaName:        "adb_restore_drill_local",
		LeaseSeconds:      3600,
	})
	if err != nil {
		t.Fatalf("Provision local schema: %v", err)
	}

	attempt, err := st.PlanRestoreDrillDryRun(ctx, id, &recordingProvisionRunner{})
	if err != nil {
		t.Fatalf("PlanRestoreDrillDryRun: %v", err)
	}
	if attempt.Kind != "restore_drill_dry_run" || attempt.Status != "succeeded" {
		t.Fatalf("attempt = %#v", attempt)
	}
	if !strings.Contains(strings.Join(attempt.Command, " "), "pg_restore --list") {
		t.Fatalf("restore drill command = %#v", attempt.Command)
	}
	if _, err := st.Archive(ctx, id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := st.Delete(ctx, id); !errors.Is(err, ErrRestoreRequired) {
		t.Fatalf("delete after dry-run err = %v, want ErrRestoreRequired", err)
	}
}

func containsDeployment(deployments []Deployment, id string) bool {
	for _, dep := range deployments {
		if dep.DeploymentID == id {
			return true
		}
	}
	return false
}

func containsAttempt(attempts []ProvisionAttempt, id string, kind string) bool {
	for _, attempt := range attempts {
		if attempt.DeploymentID == id && attempt.Kind == kind {
			return true
		}
	}
	return false
}

func containsBlocked(blocked []LifecycleBlocked, id string, reason string) bool {
	for _, item := range blocked {
		if item.DeploymentID == id && item.Reason == reason {
			return true
		}
	}
	return false
}

func containsBackupStatus(backups []Backup, status string) bool {
	for _, backup := range backups {
		if backup.Status == status {
			return true
		}
	}
	return false
}

type recordingProvisionRunner struct {
	commands []string
}

func (r *recordingProvisionRunner) Run(
	_ context.Context,
	command ProviderCommand,
) ProvisionRunResult {
	joined := strings.Join(command.Args, " ")
	r.commands = append(r.commands, joined)
	return ProvisionRunResult{
		ExitCode: 0,
		Stdout:   "dry run: " + joined,
	}
}

type failingProvisionRunner struct{}

func (failingProvisionRunner) Run(
	context.Context,
	ProviderCommand,
) ProvisionRunResult {
	return ProvisionRunResult{
		ExitCode: 1,
		Stderr:   "simulated provider failure",
	}
}
