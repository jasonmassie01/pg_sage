package agentdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type lifecycleSnapshot struct {
	status, claim, teardown, mutation string
	version                           int64
	pinged                            bool
}

func readLifecycleSnapshot(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string,
) lifecycleSnapshot {
	t.Helper()
	var snap lifecycleSnapshot
	if err := pool.QueryRow(ctx, `
		SELECT status, cleanup_claim_id, teardown_operation_id,
			provider_mutation_id, lifecycle_version, last_ping_at IS NOT NULL
		FROM sage.agent_db_deployments WHERE deployment_id=$1`, id,
	).Scan(&snap.status, &snap.claim, &snap.teardown, &snap.mutation,
		&snap.version, &snap.pinged); err != nil {
		t.Fatalf("read lifecycle snapshot: %v", err)
	}
	return snap
}

func seedArchivedTeardownDeployment(
	t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool, id string,
) {
	t.Helper()
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET status='archived', cleanup_claim_id='cleanup_x:'||deployment_id,
			teardown_operation_id='teardown_fixture',
			provisioning_status='destroying', last_ping_at=NULL
		WHERE deployment_id=$1`, id); err != nil {
		t.Fatalf("seed archived teardown: %v", err)
	}
}

// G8-B01: a ping may only record liveness; it must never rewrite lifecycle.
func TestAgentPingCannotChangeLifecycleStatus(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_ping_lifecycle"
	seedArchivedTeardownDeployment(t, st, ctx, pool, id)
	before := readLifecycleSnapshot(t, ctx, pool, id)
	for _, status := range []string{"deleted", "archived", "foo", "active", ""} {
		_, err := st.Ping(ctx, id, PingRequest{Status: status})
		valid := status == "" || status == "active"
		if valid && err != nil {
			t.Fatalf("ping status %q: unexpected error %v", status, err)
		}
		if !valid && !errors.Is(err, ErrInvalid) {
			t.Fatalf("ping status %q: err = %v, want ErrInvalid", status, err)
		}
		after := readLifecycleSnapshot(t, ctx, pool, id)
		if after.status != "archived" || after.claim != before.claim ||
			after.teardown != before.teardown {
			t.Fatalf("ping %q rewrote lifecycle: before=%+v after=%+v",
				status, before, after)
		}
	}
	if !readLifecycleSnapshot(t, ctx, pool, id).pinged {
		t.Fatal("valid ping did not record last_ping_at")
	}
	var agentStatus string
	if err := pool.QueryRow(ctx, `SELECT agent_status FROM sage.agent_db_deployments
		WHERE deployment_id=$1`, id).Scan(&agentStatus); err != nil {
		t.Fatalf("read agent_status: %v", err)
	}
	if agentStatus != "healthy" {
		t.Fatalf("agent_status = %q, want healthy", agentStatus)
	}
}

// G8-B01/B29: a heartbeat must not clear budget_exceeded.
func TestAgentPingDoesNotClearBudgetExceeded(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_ping_budget"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET status='budget_exceeded' WHERE deployment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	dep, err := st.Ping(ctx, id, PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if dep.Status != "budget_exceeded" {
		t.Fatalf("status after heartbeat = %q, want budget_exceeded", dep.Status)
	}
}

// G8-B01: the claim sweep still selects a deployment an agent keeps pinging.
func TestPingedExpiredDeploymentIsStillClaimed(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_ping_claim"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := st.Ping(ctx, id, PingRequest{Status: "foo"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid ping err = %v, want ErrInvalid", err)
	}
	claimed, err := st.ArchiveExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	if !containsDeployment(claimed, id) {
		t.Fatalf("pinged expired deployment %s escaped the TTL claim", id)
	}
}

// G8-B03: re-registering a live deployment must not wipe its identity.
func TestRegisterSameIDDoesNotResetLiveDeployment(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_register_live"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	before := readLifecycleSnapshot(t, ctx, pool, id)
	dep, err := st.Provision(ctx, RegisterRequest{
		DeploymentID: id, TenantID: "tenant_agentdb_test", AgentID: "agent_cleanup",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, LeaseSeconds: 60,
	})
	if err != nil {
		t.Fatalf("idempotent re-register: %v", err)
	}
	if dep.ProviderResourceID != "live-resource" || !dep.LiveMode ||
		dep.ProvisioningStatus != "available" {
		t.Fatalf("re-register returned reset deployment: %#v", dep)
	}
	current, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if current.ProviderResourceID != "live-resource" || !current.LiveMode ||
		current.ProvisioningStatus != "available" ||
		current.LifecycleVersion != before.version {
		t.Fatalf("re-register mutated live deployment: %#v", current)
	}
}

// G8-B03: a different tenant can never take over an existing deployment id.
func TestRegisterSameIDDifferentTenantConflicts(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_register_tenant"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID: id, TenantID: "tenant_other", AgentID: "agent_other",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-tenant register err = %v, want ErrConflict", err)
	}
	current, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if current.TenantID != "tenant_agentdb_test" || current.AgentID != "agent_cleanup" {
		t.Fatalf("ownership transferred: tenant=%s agent=%s",
			current.TenantID, current.AgentID)
	}
}

// G8-B03: a pre-live plan with the same owner may still be re-planned.
func TestRegisterSameOwnerCanReplanBeforeLive(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_register_replan"
	cleanupDeployment(t, ctx, pool, id)
	req := RegisterRequest{
		DeploymentID: id, TenantID: "tenant_agentdb_test", AgentID: "agent_plan",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, LeaseSeconds: 600,
	}
	if _, err := st.Provision(ctx, req); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	req.BudgetUSD = 42
	dep, err := st.Provision(ctx, req)
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if dep.BudgetUSD != 42 || dep.ProvisioningStatus != "planned" {
		t.Fatalf("re-plan result = budget %v status %s", dep.BudgetUSD, dep.ProvisioningStatus)
	}
}

func cleanupDeployment(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	t.Cleanup(func() { deleteDeploymentFresh(t, id) })
}

// deleteDeploymentFresh uses its own pool: callers close theirs via defer,
// which runs before t.Cleanup, so leftover rows used to leak into later
// reconcile tests.
func deleteDeploymentFresh(t *testing.T, id string) {
	pool, err := pgxpool.New(context.Background(), agentDBTestDSN())
	if err != nil {
		t.Errorf("cleanup %s: %v", id, err)
		return
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id); err != nil {
		t.Errorf("cleanup %s: %v", id, err)
	}
}
