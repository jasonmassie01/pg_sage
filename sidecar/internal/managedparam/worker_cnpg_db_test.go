package managedparam

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-12 on real PostgreSQL: the managed-change worker for a
// CloudNativePG Cluster. The test plays the operator: it changes the
// running value with ALTER SYSTEM (CNPG writes postgresql.conf) and the
// fake resolver returns the Cluster's spec.postgresql.parameters.

const cnpgResource = "cnpg|cluster=db/pg-main"

func newCNPGWorker(t *testing.T, pool *pgxpool.Pool, database, resource string,
	r Resolver) *Worker {
	t.Helper()
	store := NewStore(pool)
	w, err := NewWorker(WorkerOptions{Database: database, Provider: "cnpg", Pool: pool,
		Store: store, Resolver: r, Resource: resource, TargetTTL: time.Nanosecond})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if resource != "" {
		t.Cleanup(JoinPeers(resource, database, store))
	}
	return w
}

// operatorSets makes PostgreSQL run work_mem = value, as the operator does
// after reconciling the Cluster, and waits until the pool sees it.
func operatorSets(t *testing.T, pool *pgxpool.Pool, value, wantSetting string) {
	t.Helper()
	ctx := context.Background()
	stmt := "ALTER SYSTEM RESET work_mem"
	if value != "" {
		stmt = "ALTER SYSTEM SET work_mem = '" + value + "'"
	}
	for _, s := range []string{stmt, "SELECT pg_reload_conf()"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for runningSetting(t, pool, "work_mem") != wantSetting {
		if time.Now().After(deadline) {
			t.Fatalf("work_mem never ran %s", wantSetting)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Nil/empty: without a resolver the proposal carries placeholders and no
// drift is reported.
func TestWorkerCNPGWithoutResolverUsesPlaceholders(t *testing.T) {
	pool, ctx := testPool(t)
	w := newCNPGWorker(t, pool, "orders", "", nil)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "cnpg", "work_mem", "48MB"))
	res, err := w.Cycle(ctx)
	if err != nil || res.Proposed != 1 || len(res.Drift) != 0 ||
		res.TargetError == "" {
		t.Fatalf("cycle = %+v, %v", res, err)
	}
	p := onlyRecord(t, w.store).Proposal
	if p.Mechanism != MechanismClusterSpec || p.Target != "<namespace>/<cluster>" ||
		!strings.Contains(p.CLI, "<cluster> -n <namespace>") {
		t.Fatalf("placeholder proposal = %+v", p)
	}
}

// State transitions: proposed, then the operator applies the patch and the
// running value is observed applied; a later spec edit back is drift.
func TestWorkerCNPGProposedAppliedThenDrift(t *testing.T) {
	pool, ctx := testPool(t)
	release, err := testdb.LockCluster(ctx, testdb.SkipUnlessLive(t), "alter_system")
	if err != nil {
		t.Fatalf("cluster lock: %v", err)
	}
	original := runningSetting(t, pool, "work_mem")
	t.Cleanup(func() {
		operatorSets(t, pool, "", original)
		release()
	})
	operatorSets(t, pool, "4MB", "4096")
	r := &fakeResolver{target: cnpgTarget(map[string]string{"work_mem": "4MB"})}
	w := newCNPGWorker(t, pool, "orders", "", r)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "cnpg", "work_mem", "24MB"))
	res := mustCycle(t, w)
	rec := onlyRecord(t, w.store)
	if res.Proposed != 1 || res.Applied != 0 || len(res.Drift) != 0 ||
		rec.Status != StatusPending || rec.Proposal.Rollback.Value != "4MB" {
		t.Fatalf("proposed: cycle %+v record %+v", res, rec)
	}
	r.set(cnpgTarget(map[string]string{"work_mem": "24MB"}), nil) // operator patches
	operatorSets(t, pool, "24MB", "24576")
	res = mustCycle(t, w)
	rec = onlyRecord(t, w.store)
	if res.Applied != 1 || rec.Status != StatusApplied || rec.AppliedAt == nil ||
		len(res.Drift) != 0 {
		t.Fatalf("applied: cycle %+v record %+v", res, rec)
	}
	r.set(cnpgTarget(map[string]string{"work_mem": "4MB"}), nil) // spec edited back
	res = mustCycle(t, w)
	if len(res.Drift) != 1 || res.Drift[0].Parameter != "work_mem" ||
		res.Drift[0].Configured != "4MB" || res.Drift[0].Running != "24576" ||
		res.Drift[0].Kind != DriftMismatch {
		t.Fatalf("drift after the revert = %+v", res.Drift)
	}
	if got := w.Drift(); len(got) != 1 {
		t.Fatalf("worker drift = %+v", got)
	}
}

// Concurrency: two fleet databases on one Cluster give one proposal per
// Cluster and parameter.
func TestWorkerCNPGPeersProposeOncePerCluster(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	target := cnpgTarget(map[string]string{})
	wA := newCNPGWorker(t, poolA, "app-a", cnpgResource, &fakeResolver{target: target})
	wB := newCNPGWorker(t, poolB, "app-b", cnpgResource, &fakeResolver{target: target})
	in := intent(t, "cnpg", "shared_buffers", "2GB")
	insertIntentFinding(t, poolA, "instance:shared_buffers", in)
	insertIntentFinding(t, poolB, "instance:shared_buffers", in)
	done := make(chan CycleResult, 2)
	for _, w := range []*Worker{wA, wB} {
		go func(w *Worker) {
			res, err := w.Cycle(ctx)
			if err != nil {
				t.Errorf("Cycle: %v", err)
			}
			done <- res
		}(w)
	}
	first, second := <-done, <-done
	if first.Proposed+second.Proposed != 1 || first.HeldByPeer+second.HeldByPeer != 1 {
		t.Fatalf("cycles %+v and %+v, want one proposal and one held", first, second)
	}
	if n := storedCount(t, poolA) + storedCount(t, poolB); n != 1 {
		t.Fatalf("%d proposals stored for one Cluster change, want 1", n)
	}
}

// Conflicting values from two databases of one Cluster are both shown and
// marked in conflict.
func TestWorkerCNPGPeerConflictIsMarked(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	target := cnpgTarget(map[string]string{})
	wA := newCNPGWorker(t, poolA, "app-a", cnpgResource, &fakeResolver{target: target})
	wB := newCNPGWorker(t, poolB, "app-b", cnpgResource, &fakeResolver{target: target})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "cnpg", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "cnpg", "shared_buffers", "3GB"))
	for _, w := range []*Worker{wA, wB, wA} {
		cycle(t, ctx, w)
	}
	a, b := onlyRecord(t, wA.store).Proposal, onlyRecord(t, wB.store).Proposal
	if a.Value != "2GB" || b.Value != "3GB" {
		t.Fatalf("values %q and %q", a.Value, b.Value)
	}
	for _, tc := range []struct {
		p     Proposal
		peer  string
		value string
	}{{a, "app-b", "3GB"}, {b, "app-a", "2GB"}} {
		if len(tc.p.Conflicts) != 1 || tc.p.Conflicts[0].Database != tc.peer ||
			tc.p.Conflicts[0].Value != tc.value || len(tc.p.Blockers) != 1 ||
			!strings.Contains(tc.p.Blockers[0], "conflict") {
			t.Errorf("proposal %s conflicts %+v blockers %q", tc.p.Value, tc.p.Conflicts,
				tc.p.Blockers)
		}
	}
}
