package agentdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// recordingDestroyRunner counts provider Destroy calls per deployment and can
// fail chosen deployments with a provider error.
type recordingDestroyRunner struct {
	fakeProviderRunner
	mu      sync.Mutex
	calls   map[string]int
	creates int
	failFor map[string]error
}

func newRecordingDestroyRunner(provider string) *recordingDestroyRunner {
	return &recordingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{provider: provider, name: "recording_" + provider},
		calls:              map[string]int{},
		failFor:            map[string]error{},
	}
}

func (r *recordingDestroyRunner) Destroy(
	_ context.Context, req ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[req.Deployment.DeploymentID]++
	if err := r.failFor[req.Deployment.DeploymentID]; err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	return ProvisionResult{Status: "destroying",
		ProviderResourceID: req.Deployment.ProviderResourceID}
}

func (r *recordingDestroyRunner) Create(
	ctx context.Context, req ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	r.creates++
	r.mu.Unlock()
	return r.fakeProviderRunner.Create(ctx, req)
}

func (r *recordingDestroyRunner) destroyCalls(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

func registryWith(runner ProviderRunner) *RunnerRegistry {
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	registry.Register(runner)
	return registry
}

func insertRestoreVerifiedBackupRow(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO sage.agent_db_backups (backup_id, deployment_id, provider, status,
			verified_at, restore_verified_at, detail)
		VALUES ('bk_'||$1, $1, 'aws_rds', 'restore_verified', now(), now(), '{}')
		ON CONFLICT (backup_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed restore-verified backup row: %v", err)
	}
}

func authorizationConsumed(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string,
) bool {
	t.Helper()
	var consumed bool
	if err := pool.QueryRow(ctx, `SELECT bool_or(consumed_at IS NOT NULL)
		FROM sage.agent_db_live_authorizations WHERE deployment_id=$1`, id,
	).Scan(&consumed); err != nil {
		t.Fatalf("read authorization consumption: %v", err)
	}
	return consumed
}

// G8-B04: a dry-run-only deployment owns nothing, so a live destroy must be
// refused before any provider call and before the authorization is burned.
func TestDestroyRefusesDerivedIdentityWithoutReceipt(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_destroy_dry_run_only"
	cleanupDeployment(t, ctx, pool, id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID: id, TenantID: "tenant_agentdb_test", AgentID: "agent_destroy",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, LeaseSeconds: 3600,
		Metadata: map[string]any{"disposable": true},
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET provisioning_status='dry_run_ready' WHERE deployment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	req := persistedLiveOpTestRequest(t, st, ctx, id, "dryonly", ProvisionOpDestroy, nil)
	_, err := st.ExecuteDestroyProvisionLive(ctx, id, runner, req)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("destroy of dry-run-only deployment err = %v, want ErrInvalid", err)
	}
	if runner.destroyCalls(id) != 0 {
		t.Fatalf("provider destroy called %d times for derived identity",
			runner.destroyCalls(id))
	}
	if authorizationConsumed(t, ctx, pool, id) {
		t.Fatal("refused destroy consumed the single-use authorization")
	}
}

// G8-B04: a recorded id without a live creation receipt is not proof of
// ownership and must not be destroyed.
func TestDestroyRequiresLiveCreationReceipt(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_destroy_no_receipt"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.agent_db_creation_receipts
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	req := persistedLiveOpTestRequest(t, st, ctx, id, "noreceipt", ProvisionOpDestroy, nil)
	if _, err := st.ExecuteDestroyProvisionLive(ctx, id, runner, req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("destroy without receipt err = %v, want ErrInvalid", err)
	}
	if runner.destroyCalls(id) != 0 {
		t.Fatal("provider destroy called without a live creation receipt")
	}
}

// G8-B12: require_backup_before_destroy=false must be honoured for the
// authorized destroy path instead of failing after consuming the auth.
func TestRequireBackupFalseHonoredForAuthorizedDestroy(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_destroy_backup_optional"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	req := persistedLiveOpTestRequest(t, st, ctx, id, "nobackup", ProvisionOpDestroy,
		func(p *LiveProvisionPolicy) { p.RequireBackupBeforeDrop = false })
	attempt, err := st.ExecuteDestroyProvisionLive(ctx, id, runner, req)
	if err != nil {
		t.Fatalf("destroy with backup gate disabled: %v", err)
	}
	if attempt.Kind != "destroy_live" || runner.destroyCalls(id) != 1 {
		t.Fatalf("attempt=%#v destroy calls=%d", attempt, runner.destroyCalls(id))
	}
}

// G8-B27: an undestroyable state must be rejected before the authorization
// is consumed so the operator can retry once the state settles.
func TestDestroyValidatesStateBeforeConsumingAuthorization(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_destroy_state_first"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET provisioning_status='provisioning' WHERE deployment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	req := persistedLiveOpTestRequest(t, st, ctx, id, "state", ProvisionOpDestroy, nil)
	if _, err := st.ExecuteDestroyProvisionLive(ctx, id, runner, req); err == nil {
		t.Fatal("destroy of provisioning deployment succeeded")
	}
	if authorizationConsumed(t, ctx, pool, id) {
		t.Fatal("state rejection consumed the single-use authorization")
	}
}

// G8-B02 scenario 2: the "Cleanup expired" button archives without tearing
// down; the next reconcile pass must still destroy the live resource.
func TestCleanupArchiveDoesNotExemptFromTeardown(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_cleanup_then_reconcile"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	if _, err := st.ArchiveExpired(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	result, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(), registryWith(runner))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runner.destroyCalls(id) != 1 || !containsAttemptFor(result.DestroyLive, id) {
		t.Fatalf("archived live deployment not torn down: calls=%d result=%#v",
			runner.destroyCalls(id), result)
	}
}

// G8-B02 scenario 1: a blocked teardown is revisited once the block clears,
// and the block reason is visible on the row meanwhile.
func TestArchivedBlockedDeploymentIsRetriedAfterRestoreVerified(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_blocked_then_retried"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	registry := registryWith(runner)
	first, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(), registry)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !containsBlockedReason(first.Blocked, id, "verified restore required") {
		t.Fatalf("first pass blocked = %#v", first.Blocked)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT teardown_blocked_reason
		FROM sage.agent_db_deployments WHERE deployment_id=$1`, id).Scan(&reason); err != nil {
		t.Fatalf("read block reason: %v", err)
	}
	if !strings.Contains(reason, "verified restore required") {
		t.Fatalf("teardown_blocked_reason = %q", reason)
	}
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	if _, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(), registry); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if runner.destroyCalls(id) != 1 {
		t.Fatalf("blocked archived deployment never retried: calls=%d", runner.destroyCalls(id))
	}
}

// G8-B02 scenario 4: one provider error must not drop the rest of the batch.
func TestReconcileContinuesAfterProviderError(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	ids := []string{"adb_fix_batch_1", "adb_fix_batch_2", "adb_fix_batch_3"}
	for i, id := range ids {
		seedExpiredLiveDeployment(t, st, ctx, pool, id)
		insertRestoreVerifiedBackupRow(t, ctx, pool, id)
		if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
			SET lease_expires_at=now()-make_interval(hours => $2)
			WHERE deployment_id=$1`, id, 10-i); err != nil {
			t.Fatal(err)
		}
	}
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	runner.failFor[ids[1]] = providerError(ProviderAWSRDS, ProviderErrThrottle, "slow down", "")
	result, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(), registryWith(runner))
	if err != nil {
		t.Fatalf("reconcile aborted on one provider error: %v", err)
	}
	for _, id := range []string{ids[0], ids[2]} {
		if runner.destroyCalls(id) != 1 || !containsAttemptFor(result.DestroyLive, id) {
			t.Fatalf("%s not destroyed after sibling failure: result=%#v", id, result)
		}
	}
	if !containsBlockedID(result.Blocked, ids[1]) {
		t.Fatalf("failing deployment not reported blocked: %#v", result.Blocked)
	}
}

// G8-B02 scenario 3: a manually archived live deployment is torn down once
// its lease expires.
func TestManualArchiveLiveDeploymentIsTornDown(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_manual_archive"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	if _, err := st.Archive(ctx, id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	if _, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(),
		registryWith(runner)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runner.destroyCalls(id) != 1 {
		t.Fatalf("manually archived live deployment leaked: calls=%d", runner.destroyCalls(id))
	}
}

// G8-B02: a live deployment must never be "destroyed" by the dry-run runner.
func TestReconcileBlocksLiveDeploymentWithoutLiveRunner(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_live_no_runner"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	result, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(),
		DefaultRunnerRegistry())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !containsBlockedReason(result.Blocked, id, "live runner unavailable") {
		t.Fatalf("blocked = %#v", result.Blocked)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ProvisioningStatus != "available" || dep.TeardownOperationID != "" {
		t.Fatalf("live deployment marked as dry-run destroyed: %s/%s",
			dep.ProvisioningStatus, dep.TeardownOperationID)
	}
}

// G8-B18: emergency stop blocks autonomous TTL teardown and live creates.
func TestEmergencyStopBlocksAgentDBMutations(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	setEmergencyStopForTest(t, ctx, pool)
	id := "adb_fix_estop_teardown"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	insertRestoreVerifiedBackupRow(t, ctx, pool, id)
	runner := newRecordingDestroyRunner(ProviderAWSRDS)
	result, err := st.ReconcileAbandonedDeployments(ctx, time.Now().UTC(), registryWith(runner))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runner.destroyCalls(id) != 0 ||
		!containsBlockedReason(result.Blocked, id, "emergency stop") {
		t.Fatalf("emergency stop ignored: calls=%d blocked=%#v",
			runner.destroyCalls(id), result.Blocked)
	}
	createID := "adb_fix_estop_create"
	cleanupDeployment(t, ctx, pool, createID)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID: createID, TenantID: "tenant_agentdb_test", AgentID: "agent_estop",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, LeaseSeconds: 3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, createID); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	req := persistedLiveTestRequest(t, st, ctx, createID, "estop")
	_, err = st.ExecuteProvisionLive(ctx, createID, runner, req)
	if err == nil || !strings.Contains(err.Error(), "emergency stop") || runner.creates != 0 {
		t.Fatalf("live create under emergency stop: err=%v creates=%d", err, runner.creates)
	}
}

func setEmergencyStopForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, sql := range []string{
		`CREATE TABLE IF NOT EXISTS sage.config (key text NOT NULL, value text NOT NULL,
			database_id integer, updated_at timestamptz, updated_by text)`,
		`DELETE FROM sage.config WHERE key='emergency_stop'`,
		`INSERT INTO sage.config (key, value, updated_at, updated_by)
			VALUES ('emergency_stop', 'true', now(), 'test')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("set emergency stop: %v", err)
		}
	}
	// The caller's pool is closed by defer before t.Cleanup runs.
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), agentDBTestDSN())
		if err != nil {
			t.Errorf("clear emergency stop: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(),
			`DELETE FROM sage.config WHERE key='emergency_stop'`); err != nil {
			t.Errorf("clear emergency stop: %v", err)
		}
	})
}

func containsAttemptFor(attempts []ProvisionAttempt, id string) bool {
	for _, attempt := range attempts {
		if attempt.DeploymentID == id {
			return true
		}
	}
	return false
}

func containsBlockedID(blocked []LifecycleBlocked, id string) bool {
	for _, item := range blocked {
		if item.DeploymentID == id {
			return true
		}
	}
	return false
}

func containsBlockedReason(blocked []LifecycleBlocked, id, reason string) bool {
	for _, item := range blocked {
		if item.DeploymentID == id && strings.Contains(item.Reason, reason) {
			return true
		}
	}
	return false
}
