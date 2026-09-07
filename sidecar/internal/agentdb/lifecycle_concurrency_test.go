package agentdb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestExpiredCleanupClaimIsInvalidatedByLeaseRenewal(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_lease_wins"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)

	claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	claim := deploymentByID(t, claimed, id)
	if claim.CleanupClaimID == "" || claim.LifecycleVersion == 0 {
		t.Fatalf("claim lacks durable identity: %#v", claim)
	}

	extended, err := st.ExtendLease(ctx, id, LeaseRequest{
		LeaseSeconds: 3600,
		Reason:       "agent is still active",
	})
	if err != nil {
		t.Fatalf("ExtendLease: %v", err)
	}
	if extended.Status != "active" || extended.CleanupClaimID != "" {
		t.Fatalf("renewed deployment kept cleanup claim: %#v", extended)
	}
	if _, err := st.authorizeTeardownClaim(ctx, claim, time.Now().UTC()); !errors.Is(err, ErrConflict) {
		t.Fatalf("authorize stale claim error = %v, want ErrConflict", err)
	}

	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.ProvisioningStatus != "available" {
		t.Fatalf("renewed deployment provisioning status = %s", dep.ProvisioningStatus)
	}
}

func TestArchiveExpiredClaimsEachDeploymentAtMostOnce(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_single_claim"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)

	start := make(chan struct{})
	results := make(chan []Deployment, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
			results <- claimed
			errs <- err
		}()
	}
	close(start)

	total := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ArchiveExpired: %v", err)
		}
		for _, dep := range <-results {
			if dep.DeploymentID == id {
				total++
			}
		}
	}
	if total != 1 {
		t.Fatalf("deployment claimed %d times, want exactly once", total)
	}
}

func TestConcurrentAbandonedReconcilersDestroyAtMostOnce(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_single_destroy"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	runner := &countingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "counting_rds",
		},
	}
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	registry.Register(runner)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := st.ReconcileAbandonedDeployments(
				ctx, time.Now().UTC(), registry,
			)
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ReconcileAbandonedDeployments: %v", err)
		}
	}
	if got := runner.destroyCount(id); got != 1 {
		t.Fatalf("provider Destroy called %d times, want exactly once", got)
	}
	if runner.operationID(id) == "" {
		t.Fatal("provider destroy request lacked durable operation ID")
	}
}

func TestStaleCleanupVersionAndPolicyBlockDestroy(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_stale_policy"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	claim := deploymentByID(t, claimed, id)

	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET safety_mode='observation', lifecycle_version=lifecycle_version+1,
			updated_at=now()
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("change safety policy: %v", err)
	}
	if _, err := st.authorizeTeardownClaim(ctx, claim, time.Now().UTC()); !errors.Is(err, ErrConflict) {
		t.Fatalf("authorize stale policy claim error = %v, want ErrConflict", err)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.ProvisioningStatus != "available" {
		t.Fatalf("stale claim advanced provisioning state to %s", dep.ProvisioningStatus)
	}
}

func TestAuthorizedDestroyRetryUsesStableOperationID(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_stable_operation"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	claim := deploymentByID(t, claimed, id)
	authorized, err := st.authorizeTeardownClaim(ctx, claim, time.Now().UTC())
	if err != nil {
		t.Fatalf("authorizeTeardownClaim: %v", err)
	}
	runner := &countingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "retrying_rds",
		},
	}

	if _, err := st.destroyAuthorizedLive(ctx, authorized, runner); err != nil {
		t.Fatalf("first destroyAuthorizedLive: %v", err)
	}
	if _, err := st.destroyAuthorizedLive(ctx, authorized, runner); err != nil {
		t.Fatalf("retry destroyAuthorizedLive: %v", err)
	}
	if runner.destroyCount(id) != 2 {
		t.Fatalf("provider Destroy calls = %d, want 2", runner.destroyCount(id))
	}
	ids := runner.operationIDs(id)
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("operation IDs = %#v, want one stable non-empty ID", ids)
	}
}

func TestLiveReconcileRetriesUncertainDestroyWithStableOperationID(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_retry_uncertain"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	runner := &countingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "uncertain_rds",
		},
		failures: 1,
	}
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	registry.Register(runner)

	if _, err := st.ReconcileAbandonedDeployments(
		ctx, time.Now().UTC(), registry,
	); err == nil {
		t.Fatal("first reconcile succeeded despite uncertain provider destroy")
	}
	if _, err := st.ReconcileLiveProvisioning(ctx, registry); err != nil {
		t.Fatalf("ReconcileLiveProvisioning retry: %v", err)
	}
	ids := runner.operationIDs(id)
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("retry operation IDs = %#v, want two identical non-empty IDs", ids)
	}
}

func TestConcurrentAuthorizedDestroyAllowsOneInFlightMutation(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_single_inflight"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	authorized, err := st.authorizeTeardownClaim(
		ctx, deploymentByID(t, claimed, id), time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("authorizeTeardownClaim: %v", err)
	}
	runner := newBlockingDestroyRunner()

	firstDone := make(chan error, 1)
	go func() {
		_, destroyErr := st.destroyAuthorizedLive(ctx, authorized, runner)
		firstDone <- destroyErr
	}()
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first provider mutation did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, destroyErr := st.destroyAuthorizedLive(ctx, authorized, runner)
		secondDone <- destroyErr
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("concurrent destroy error = %v, want ErrRateLimited", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent destroy waited instead of failing closed")
	}
	if got := runner.destroyCount(); got != 1 {
		t.Fatalf("in-flight provider Destroy calls = %d, want 1", got)
	}
	close(runner.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first destroyAuthorizedLive: %v", err)
	}
	if _, err := st.destroyAuthorizedLive(ctx, authorized, runner); err != nil {
		t.Fatalf("sequential retry: %v", err)
	}
	if got := runner.destroyCount(); got != 2 {
		t.Fatalf("provider Destroy calls after retry = %d, want 2", got)
	}
}

func TestConcurrentDirectDestroyUsesOneDurableOperation(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_direct_destroy_single_inflight"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	if _, err := st.ExtendLease(ctx, id, LeaseRequest{LeaseSeconds: 3600}); err != nil {
		t.Fatalf("ExtendLease: %v", err)
	}
	runner := newBlockingDestroyRunner()

	firstDone := make(chan error, 1)
	go func() {
		_, destroyErr := st.DestroyProvisionLive(ctx, id, runner)
		firstDone <- destroyErr
	}()
	select {
	case <-runner.entered:
	case err := <-firstDone:
		t.Fatalf("first direct destroy exited before provider call: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first direct provider mutation did not start")
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get during direct destroy: %v", err)
	}
	if dep.TeardownOperationID == "" {
		t.Fatal("direct destroy did not persist its operation ID")
	}
	if _, err := st.DestroyProvisionLive(ctx, id, runner); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("concurrent direct destroy error = %v, want ErrRateLimited", err)
	}
	if got := runner.destroyCount(); got != 1 {
		t.Fatalf("in-flight direct provider calls = %d, want 1", got)
	}
	close(runner.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first direct destroy: %v", err)
	}
	if _, err := st.DestroyProvisionLive(ctx, id, runner); err != nil {
		t.Fatalf("direct sequential retry: %v", err)
	}
	ids := runner.operationIDs()
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("direct destroy operation IDs = %#v, want stable retry ID", ids)
	}
}

func TestLiveReconcileResumesUncertainDirectDestroy(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_direct_destroy_reconcile_retry"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	if _, err := st.ExtendLease(ctx, id, LeaseRequest{LeaseSeconds: 3600}); err != nil {
		t.Fatalf("ExtendLease: %v", err)
	}
	runner := &countingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "direct_retry_rds",
		},
		failures: 1,
	}
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	registry.Register(runner)

	if _, err := st.DestroyProvisionLive(ctx, id, runner); err == nil {
		t.Fatal("direct destroy succeeded despite uncertain provider result")
	}
	if _, err := st.ReconcileLiveProvisioning(ctx, registry); err != nil {
		t.Fatalf("ReconcileLiveProvisioning: %v", err)
	}
	ids := runner.operationIDs(id)
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("direct reconcile operation IDs = %#v, want stable retry ID", ids)
	}
}

func TestExpiredProviderMutationLeaseCanBeReclaimed(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cleanup_reclaim_mutation"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	seedRestoreVerifiedBackup(t, st, ctx, id)
	claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	authorized, err := st.authorizeTeardownClaim(
		ctx, deploymentByID(t, claimed, id), time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("authorizeTeardownClaim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provider_mutation_id='crashed-controller',
			provider_mutation_expires_at=now()-interval '1 minute'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("seed expired mutation lease: %v", err)
	}
	runner := &countingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "recovery_rds",
		},
	}
	if _, err := st.destroyAuthorizedLive(ctx, authorized, runner); err != nil {
		t.Fatalf("destroyAuthorizedLive after lease expiry: %v", err)
	}
	if got := runner.destroyCount(id); got != 1 {
		t.Fatalf("provider calls after lease recovery = %d, want 1", got)
	}
}

func TestConcurrentLiveCreateAllowsOneInFlightProviderMutation(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_create_single_inflight"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_create",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		LeaseSeconds:      3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("PreflightProvision: %v", err)
	}
	runner := newBlockingCreateRunner()
	req := persistedLiveTestRequest(t, st, ctx, id, "single-inflight")
	firstDone := make(chan error, 1)
	go func() {
		_, createErr := st.ExecuteProvisionLive(ctx, id, runner, req)
		firstDone <- createErr
	}()
	select {
	case <-runner.entered:
	case err := <-firstDone:
		t.Fatalf("first live create exited before provider call: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first live create did not reach provider")
	}
	if _, err := st.ExecuteProvisionLive(ctx, id, runner, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent live create error = %v, want ErrConflict", err)
	}
	if got := runner.createCount(); got != 1 {
		t.Fatalf("in-flight provider Create calls = %d, want 1", got)
	}
	close(runner.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first live create: %v", err)
	}
}

func TestReconcileLiveProvisioningReleasesItsLockSession(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	registry := NewRunnerRegistry(DryRunProvisionRunner{})
	if _, err := st.ReconcileLiveProvisioning(ctx, registry); err != nil {
		t.Fatalf("ReconcileLiveProvisioning: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `
		SELECT pg_try_advisory_lock(hashtext('agentdb-live-reconcile'))`,
	).Scan(&locked); err != nil {
		t.Fatalf("try advisory lock: %v", err)
	}
	if !locked {
		t.Fatal("live reconcile left its session advisory lock held")
	}
	if _, err := conn.Exec(ctx, `
		SELECT pg_advisory_unlock(hashtext('agentdb-live-reconcile'))`); err != nil {
		t.Fatalf("unlock verification lock: %v", err)
	}
}

func TestLiveReconcileUnlockDiscardsSessionWhenLockIsNotOwned(t *testing.T) {
	_, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	conn, release, err := acquireLiveReconcileLock(ctx, pool)
	if err != nil {
		t.Fatalf("acquireLiveReconcileLock: %v", err)
	}
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	release()

	deadline := time.Now().Add(2 * time.Second)
	for {
		var sessions int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE pid=$1", pid,
		).Scan(&sessions); err != nil {
			t.Fatalf("check discarded session: %v", err)
		}
		if sessions == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unlock-failure session %d remained in the pool", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type countingDestroyRunner struct {
	fakeProviderRunner
	mu       sync.Mutex
	requests []destroyRequestRecord
	failures int
}

type destroyRequestRecord struct {
	deploymentID string
	operationID  string
}

func (r *countingDestroyRunner) Destroy(
	_ context.Context,
	req ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	r.requests = append(r.requests, destroyRequestRecord{
		deploymentID: req.Deployment.DeploymentID,
		operationID:  req.OperationID,
	})
	if r.failures > 0 {
		r.failures--
		r.mu.Unlock()
		return ProvisionResult{
			Status: "status_unknown",
			Error:  errors.New("provider result uncertain"),
		}
	}
	r.mu.Unlock()
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: req.Deployment.ProviderResourceID,
	}
}

func (r *countingDestroyRunner) destroyCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, req := range r.requests {
		if req.deploymentID == id {
			count++
		}
	}
	return count
}

func (r *countingDestroyRunner) operationID(id string) string {
	ids := r.operationIDs(id)
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func (r *countingDestroyRunner) operationIDs(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := []string{}
	for _, req := range r.requests {
		if req.deploymentID == id {
			ids = append(ids, req.operationID)
		}
	}
	return ids
}

type blockingDestroyRunner struct {
	fakeProviderRunner
	mu      sync.Mutex
	count   int
	ids     []string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingDestroyRunner() *blockingDestroyRunner {
	return &blockingDestroyRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "blocking_rds",
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *blockingDestroyRunner) Destroy(
	_ context.Context,
	req ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	r.count++
	r.ids = append(r.ids, req.OperationID)
	r.mu.Unlock()
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: req.Deployment.ProviderResourceID,
	}
}

func (r *blockingDestroyRunner) destroyCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func (r *blockingDestroyRunner) operationIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

type blockingCreateRunner struct {
	fakeProviderRunner
	mu      sync.Mutex
	count   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingCreateRunner() *blockingCreateRunner {
	return &blockingCreateRunner{
		fakeProviderRunner: fakeProviderRunner{
			provider: ProviderAWSRDS,
			name:     "blocking_create_rds",
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *blockingCreateRunner) Create(
	_ context.Context,
	_ ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	return ProvisionResult{
		Status:             "available",
		ProviderResourceID: "created-resource",
	}
}

func (r *blockingCreateRunner) createCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func seedExpiredLiveDeployment(
	t *testing.T,
	st *Store,
	ctx context.Context,
	pool *pgxpool.Pool,
	id string,
) {
	t.Helper()
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	})
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:       id,
		TenantID:           "tenant_agentdb_test",
		AgentID:            "agent_cleanup",
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
			provisioning_status='available', live_mode=true,
			provider_resource_id='live-resource'
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("expire live deployment: %v", err)
	}
}

func seedRestoreVerifiedBackup(t *testing.T, st *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := st.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_" + id,
		Provider: ProviderAWSRDS,
		Status:   "restore_verified",
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
}

func deploymentByID(t *testing.T, deployments []Deployment, id string) Deployment {
	t.Helper()
	for _, dep := range deployments {
		if dep.DeploymentID == id {
			return dep
		}
	}
	t.Fatalf("deployment %s not found in %#v", id, deployments)
	return Deployment{}
}
