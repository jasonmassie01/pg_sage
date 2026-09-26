package agentdb

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// ambiguousCreateRunner fails its create ambiguously and records whether the
// create operation id was durable before the provider call.
type ambiguousCreateRunner struct {
	fakeProviderRunner
	store          *Store
	mu             sync.Mutex
	creates        int
	operationSeen  string
	statusResult   ProvisionResult
	createStatus   string
	statusRequests []ProvisionRequest
}

func (r *ambiguousCreateRunner) Create(
	ctx context.Context, req ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates++
	if dep, err := r.store.Get(ctx, req.Deployment.DeploymentID); err == nil {
		r.operationSeen = dep.CreateOperationID
	}
	return ProvisionResult{Status: r.createStatus,
		Error: errors.New("read tcp: connection reset by peer")}
}

func (r *ambiguousCreateRunner) Status(
	_ context.Context, req ProvisionRequest,
) ProvisionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusRequests = append(r.statusRequests, req)
	return r.statusResult
}

func seedPlannedLiveCandidate(t *testing.T, st *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := st.Provision(ctx, RegisterRequest{
		DeploymentID: id, TenantID: "tenant_agentdb_test", AgentID: "agent_ambiguous",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, LeaseSeconds: 3600,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := st.PreflightProvision(ctx, id); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}

// G8-B06: an ambiguous create is recorded before the call, stays
// create_uncertain, cannot be blindly retried, and is adopted by lookup.
func TestAmbiguousCreateStaysUncertainAndIsReconciled(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_ambiguous_create"
	cleanupDeployment(t, ctx, pool, id)
	seedPlannedLiveCandidate(t, st, ctx, id)
	runner := &ambiguousCreateRunner{
		fakeProviderRunner: fakeProviderRunner{provider: ProviderAWSRDS, name: "ambiguous_rds"},
		store:              st, createStatus: "create_uncertain",
	}
	req := persistedLiveTestRequest(t, st, ctx, id, "ambiguous-1")
	if _, err := st.ExecuteProvisionLive(ctx, id, runner, req); err == nil {
		t.Fatal("ambiguous create reported success")
	}
	if runner.operationSeen == "" {
		t.Fatal("create operation id was not durable before the provider call")
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ProvisioningStatus != "create_uncertain" || !dep.LiveMode {
		t.Fatalf("after ambiguous create: status=%s live=%v", dep.ProvisioningStatus, dep.LiveMode)
	}
	retry := persistedLiveTestRequest(t, st, ctx, id, "ambiguous-2")
	if _, err := st.ExecuteProvisionLive(ctx, id, runner, retry); !errors.Is(err, ErrConflict) {
		t.Fatalf("blind retry err = %v, want ErrConflict", err)
	}
	if runner.creates != 1 {
		t.Fatalf("provider creates = %d, want 1 (no second billed resource)", runner.creates)
	}
	runner.statusResult = ProvisionResult{Status: "available",
		ProviderResourceID: "pgsage-adopted", ConnectionInfo: map[string]any{}}
	if _, err := st.ReconcileLiveProvisioning(ctx, registryWith(runner)); err != nil {
		t.Fatalf("ReconcileLiveProvisioning: %v", err)
	}
	if len(runner.statusRequests) == 0 {
		t.Fatal("reconcile did not look up the uncertain create")
	}
	dep, err = st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ProvisioningStatus != "available" || dep.ProviderResourceID != "pgsage-adopted" {
		t.Fatalf("uncertain create not adopted: %s/%s", dep.ProvisioningStatus, dep.ProviderResourceID)
	}
	if err := st.requireOwnedLiveResource(ctx, dep); err != nil {
		t.Fatalf("adopted resource has no live receipt: %v", err)
	}
}

// G8-B06: the hosted runner's deliberate status_unknown is not rewritten to
// the retryable failed state.
func TestHostedStatusUnknownCreateIsNotRetryable(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_status_unknown_create"
	cleanupDeployment(t, ctx, pool, id)
	seedPlannedLiveCandidate(t, st, ctx, id)
	runner := &ambiguousCreateRunner{
		fakeProviderRunner: fakeProviderRunner{provider: ProviderAWSRDS, name: "unknown_rds"},
		store:              st, createStatus: "status_unknown",
	}
	req := persistedLiveTestRequest(t, st, ctx, id, "unknown-1")
	_, _ = st.ExecuteProvisionLive(ctx, id, runner, req)
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ProvisioningStatus == "failed" {
		t.Fatal("ambiguous create became retryable 'failed'")
	}
}

// G8-B23: a destroying row without a teardown id is resolved to destroyed
// when the provider reports the resource gone, instead of blocking forever.
func TestReconcileResolvesDestroyingWithoutTeardownID(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_destroying_no_teardown"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET provisioning_status='destroying', teardown_operation_id=''
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	runner := notFoundStatusRunner{fakeProviderRunner{provider: ProviderAWSRDS, name: "gone_rds"}}
	result, err := st.ReconcileLiveProvisioning(ctx, registryWith(runner))
	if err != nil {
		t.Fatalf("ReconcileLiveProvisioning: %v", err)
	}
	dep, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ProvisioningStatus != "destroyed" || containsBlockedID(result.Blocked, id) {
		t.Fatalf("status=%s blocked=%#v", dep.ProvisioningStatus, result.Blocked)
	}
}
