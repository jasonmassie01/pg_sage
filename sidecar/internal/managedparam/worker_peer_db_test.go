package managedparam

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// clouds-08 on real PostgreSQL: two databases of one RDS instance, each
// with its own sage schema. A change one of them holds open is not
// proposed again by the other; a different value is.

const peerResource = "rds|host=orders.c9akciq32.us-east-1.rds.amazonaws.com:5432"

// peerPools returns database A (the package's shared test database) and a
// second, bootstrapped database B on the same server.
func peerPools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool, context.Context) {
	t.Helper()
	poolA, ctx := testPool(t)
	dsn := testdb.CreateDatabase(t, "mp_peer_b")
	poolB, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect database B: %v", err)
	}
	t.Cleanup(poolB.Close)
	if err := schema.Bootstrap(ctx, poolB); err != nil {
		t.Fatalf("bootstrap database B: %v", err)
	}
	return poolA, poolB, ctx
}

// peerWorker is database's worker on resource, joined to its peers until
// the test ends (or leave is called). A nil resolver builds placeholders.
func peerWorker(t *testing.T, pool *pgxpool.Pool, database, resource string,
	r Resolver) (*Worker, func()) {
	t.Helper()
	store := NewStore(pool)
	w, err := NewWorker(WorkerOptions{Database: database, Provider: "rds", Pool: pool,
		Store: store, Resolver: r, Resource: resource})
	if err != nil {
		t.Fatalf("NewWorker(%s): %v", database, err)
	}
	leave := JoinPeers(resource, database, store)
	t.Cleanup(leave)
	return w, leave
}

func storedCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.managed_change_proposals").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cycle(t *testing.T, ctx context.Context, w *Worker) CycleResult {
	t.Helper()
	res, err := w.Cycle(ctx)
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	return res
}

func TestWorkerPeerSkipsAChangeAPeerHolds(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, leaveA := peerWorker(t, poolA, "peer-a", peerResource,
		&fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	if res := cycle(t, ctx, wA); res.Proposed != 1 || res.HeldByPeer != 0 {
		t.Fatalf("A = %+v, want 1 proposed", res)
	}
	for range 2 { // every cycle, not only the first
		res := cycle(t, ctx, wB)
		if res.Proposed != 0 || res.Refreshed != 0 || res.HeldByPeer != 1 ||
			res.PeerError != "" {
			t.Fatalf("B = %+v, want the change held by A", res)
		}
	}
	if n := storedCount(t, poolB); n != 0 {
		t.Fatalf("B stored %d proposals for a change A holds", n)
	}
	if n := storedCount(t, poolA); n != 1 {
		t.Fatalf("A stored %d proposals, want 1", n)
	}
	leaveA() // A leaves the fleet: B owns the change now
	if res := cycle(t, ctx, wB); res.Proposed != 1 || res.HeldByPeer != 0 {
		t.Fatalf("B after A left = %+v, want 1 proposed", res)
	}
}

func TestWorkerPeerProposesADifferentValue(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "4GB"))
	cycle(t, ctx, wA)
	if res := cycle(t, ctx, wB); res.Proposed != 1 || res.HeldByPeer != 0 {
		t.Fatalf("B = %+v, a different value must be proposed", res)
	}
}

// Boundary: databases of different instances are never peers.
func TestWorkerPeerOtherInstanceIsNotAPeer(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b",
		"rds|host=billing.c9akciq32.us-east-1.rds.amazonaws.com:5432",
		&fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	if res := cycle(t, ctx, wB); res.Proposed != 1 || res.HeldByPeer != 0 {
		t.Fatalf("B on another instance = %+v, want 1 proposed", res)
	}
}

// A database that already holds its own open copy (from before peers)
// keeps refreshing it rather than leaving it stale.
func TestWorkerPeerOwnOpenCopyKeepsRefreshing(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	if _, _, err := NewStore(poolB).Upsert(ctx,
		buildProposal(t, "shared_buffers", "2GB"), 0); err != nil {
		t.Fatal(err)
	}
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	if res := cycle(t, ctx, wB); res.Refreshed != 1 || res.HeldByPeer != 0 {
		t.Fatalf("B with its own copy = %+v, want it refreshed", res)
	}
}

// A peer's recent rejection of the same change holds it too: the operator
// already said no to this instance's change.
func TestWorkerPeerRejectionHoldsTheChange(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	recs, err := NewStore(poolA).List(ctx, nil, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("A records = %v %v", recs, err)
	}
	if _, err := NewStore(poolA).Decide(ctx, recs[0].ID, false, 7, "not now"); err != nil {
		t.Fatal(err)
	}
	if res := cycle(t, ctx, wB); res.Proposed != 0 || res.HeldByPeer != 1 {
		t.Fatalf("B after A's rejection = %+v, want it held", res)
	}
}

// Without telemetry both databases build placeholder targets; the change
// is still held once.
func TestWorkerPeerPlaceholderTargetsAreHeld(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, nil)
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, nil)
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	if res := cycle(t, ctx, wB); res.Proposed != 0 || res.HeldByPeer != 1 {
		t.Fatalf("B with placeholders = %+v, want it held", res)
	}
}

// Error propagation: a peer that cannot be read does not block the
// proposal; the failure is reported once, not every intent or cycle.
func TestWorkerPeerErrorFallsBackToProposing(t *testing.T) {
	_, poolB, ctx := peerPools(t)
	broken, err := pgxpool.New(ctx, poolB.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	t.Cleanup(JoinPeers(peerResource, "peer-broken", NewStore(broken)))
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:max_wal_size",
		intent(t, "rds", "max_wal_size", "4GB"))
	res := cycle(t, ctx, wB)
	if res.Proposed != 2 || res.HeldByPeer != 0 {
		t.Fatalf("B with a broken peer = %+v, want both proposed", res)
	}
	if !strings.Contains(res.PeerError, "peer-broken") ||
		!strings.Contains(res.PeerError, "proposing") || strings.Count(res.PeerError,
		"peer-broken") != 1 {
		t.Fatalf("peer error = %q, want one actionable report naming the peer",
			res.PeerError)
	}
	insertIntentFinding(t, poolB, "instance:work_mem", intent(t, "rds", "work_mem", "64MB"))
	if res := cycle(t, ctx, wB); res.PeerError != "" || res.Proposed != 1 {
		t.Fatalf("second cycle = %+v, want the same failure not reported again", res)
	}
}

// Concurrency: A and B cycling at the same moment store the change once
// (the peer check and the upsert are one step per instance).
func TestWorkerPeerConcurrentCyclesStoreOnce(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	start := make(chan struct{})
	results := make([]CycleResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, w := range []*Worker{wA, wB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.Cycle(ctx)
		}()
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("cycles: %v / %v", errs[0], errs[1])
	}
	proposed := results[0].Proposed + results[1].Proposed
	held := results[0].HeldByPeer + results[1].HeldByPeer
	stored := storedCount(t, poolA) + storedCount(t, poolB)
	if proposed != 1 || held != 1 || stored != 1 {
		t.Fatalf("concurrent cycles: proposed %d held %d stored %d, want 1/1/1",
			proposed, held, stored)
	}
}
