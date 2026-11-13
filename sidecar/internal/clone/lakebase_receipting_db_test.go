package clone

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-03: a Lakebase branch clone is always receipted (adapter
// lakebase_branch, scope the project), so a crash between create and
// destroy leaves a record the reaper acts on.

// statusAPI is a fakeBranchAPI that records the receipt status the
// control plane saw at each delete.
type statusAPI struct {
	*fakeBranchAPI
	pool *pgxpool.Pool

	mu       sync.Mutex
	atDelete []string
}

func (s *statusAPI) DeleteBranch(ctx context.Context, name string) error {
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(status), 'none')
		FROM sage.clone_instances WHERE ref = $1`, name).Scan(&status); err != nil {
		return err
	}
	s.mu.Lock()
	s.atDelete = append(s.atDelete, status)
	s.mu.Unlock()
	return s.fakeBranchAPI.DeleteBranch(ctx, name)
}

func lakebaseReceipting(t *testing.T, api LakebaseBranchAPI, s *ReceiptStore,
	wait time.Duration) *ReceiptingProvider {
	t.Helper()
	cfg := testLakebaseConfig(api)
	cfg.Now = time.Now
	if wait > 0 {
		cfg.WaitTimeout = wait
	}
	inner := newLakebaseForTest(t, cfg)
	p, err := NewReceiptingProvider(inner, s, ReceiptingConfig{Adapter: LakebaseAdapter,
		Scope: "projects/shop", Deployment: staticDeployment(testDeployment),
		Purpose: PurposeRehearsal, TTL: 75 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLakebaseReceipting_CreatingReadyDestroyingDestroyed(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	api := &statusAPI{fakeBranchAPI: &fakeBranchAPI{}, pool: pool}
	p := lakebaseReceipting(t, api, s, 0)
	c, err := p.Create(ctx, CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatal(err)
	}
	r := onlyReceipt(t, pool)
	if r.Status != StatusReady || r.Ref != c.ID || r.Adapter != LakebaseAdapter ||
		r.Scope != "projects/shop" || r.Purpose != PurposeRehearsal {
		t.Fatalf("after create: %+v (clone %s)", r, c.ID)
	}
	if err := p.Destroy(ctx, c); err != nil {
		t.Fatal(err)
	}
	if len(api.atDelete) != 1 || api.atDelete[0] != StatusDestroying {
		t.Fatalf("receipt status when the branch was deleted: %v", api.atDelete)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroyed {
		t.Fatalf("after destroy: %+v", got)
	}
}

func TestLakebaseReceipting_DeleteBranchFailureLeavesDestroying(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	api := &statusAPI{fakeBranchAPI: &fakeBranchAPI{}, pool: pool}
	p := lakebaseReceipting(t, api, s, 0)
	c, err := p.Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	api.deleteBranchErr = errors.New("control plane unavailable")
	if err := p.Destroy(ctx, c); err == nil || !errors.Is(err, api.deleteBranchErr) {
		t.Fatalf("failed delete = %v", err)
	}
	if r := onlyReceipt(t, pool); r.Status != StatusDestroying || r.Ref != c.ID {
		t.Fatalf("receipt after a failed delete: %+v, want destroying for the reaper", r)
	}
}

func TestLakebaseReceipting_WaitPastDeadlineIsCreateUncertain(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	api := &fakeBranchAPI{blockBranch: true}
	p := lakebaseReceipting(t, api, s, 50*time.Millisecond)
	if _, err := p.Create(ctx, CloneSpec{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out create = %v", err)
	}
	if r := onlyReceipt(t, pool); r.Status != StatusCreateUncertain || r.Ref != "" {
		t.Fatalf("receipt after a timed-out create: %+v, want create_uncertain", r)
	}
	if api.branches[0].ExpireTime.IsZero() {
		t.Fatal("no server-side expiry on the timed-out branch request")
	}
}

func TestLakebaseReceipting_EndpointFailureIsFailed(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	api := &fakeBranchAPI{createEndpointErr: errors.New("quota")}
	p := lakebaseReceipting(t, api, s, 0)
	if _, err := p.Create(ctx, CloneSpec{}); err == nil {
		t.Fatal("endpoint failure created a clone")
	}
	if r := onlyReceipt(t, pool); r.Status != StatusFailed {
		t.Fatalf("receipt after a definite failure: %+v, want failed", r)
	}
}

func TestLakebaseReceipting_ConcurrentRehearsalsGetDistinctBranchesAndReceipts(
	t *testing.T) {
	s, pool, ctx := receiptStore(t)
	p := lakebaseReceipting(t, &fakeBranchAPI{}, s, 0)
	clones := make([]Clone, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range clones {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clones[i], errs[i] = p.Create(ctx, CloneSpec{})
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || clones[0].ID == clones[1].ID {
		t.Fatalf("concurrent creates: %v %v ids %q %q", errs[0], errs[1], clones[0].ID,
			clones[1].ID)
	}
	var receipts, refs int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT ref)
		FROM sage.clone_instances WHERE status = 'ready'`).Scan(&receipts, &refs); err != nil {
		t.Fatal(err)
	}
	if receipts != 2 || refs != 2 {
		t.Fatalf("ready receipts %d with %d distinct refs, want 2 and 2", receipts, refs)
	}
}

// The reaper destroys an expired Lakebase receipt by its ref alone.
func TestLakebaseReaper_DestroysAnExpiredBranchByRef(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	api := &fakeBranchAPI{}
	p := lakebaseReceipting(t, api, s, 0)
	c, err := p.Create(ctx, CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	cfg := testLakebaseConfig(api)
	cfg.Production = nil // the reaper never creates
	report, err := Reaper{Store: s, Provider: newLakebaseForTest(t, cfg),
		Scope: ReceiptScope{DeploymentID: testDeployment, Adapter: LakebaseAdapter,
			Scope: "projects/shop"}, Purpose: PurposeRehearsal}.Reap(ctx)
	if err != nil || len(report.Destroyed) != 1 || len(report.Failed) != 0 {
		t.Fatalf("reap = %+v, %v", report, err)
	}
	calls := api.callList()
	if last := calls[len(calls)-1]; last != "delete_branch "+c.ID {
		t.Fatalf("calls = %q, want the expired branch deleted", calls)
	}
}
