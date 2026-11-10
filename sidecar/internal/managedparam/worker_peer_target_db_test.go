package managedparam

import (
	"errors"
	"testing"
)

// clouds-08 review: a change's identity is its fingerprint (provider,
// parameter, value), not its target, so a database's own copy built
// against an older target, and a placeholder copy built while the target
// is unknown, are still the same change.

// A database whose own open copy names an older parameter group keeps
// refreshing it to the current target, even while a peer holds the change.
func TestWorkerPeerOwnCopyWithOldTargetKeepsRefreshing(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	old := rdsTarget()
	old.ParameterGroup = "orders-pg16-old"
	stale, err := Build(intent(t, "rds", "shared_buffers", "2GB"), old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore(poolB).Upsert(ctx, stale, 0); err != nil {
		t.Fatal(err)
	}
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	res := cycle(t, ctx, wB)
	if res.Refreshed != 1 || res.HeldByPeer != 0 || res.Proposed != 0 {
		t.Fatalf("B with its own copy on an old target = %+v, want it refreshed", res)
	}
	recs, err := NewStore(poolB).List(ctx, []string{StatusPending}, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("B open records = %v %v, want 1", recs, err)
	}
	want := buildProposal(t, "shared_buffers", "2GB").Target
	if recs[0].Proposal.Target != want {
		t.Fatalf("B's copy targets %q, want the current %q", recs[0].Proposal.Target, want)
	}
}

// A database that cannot resolve the target builds placeholders; a peer
// holding the resolved change still holds it, so no placeholder duplicate
// of a real command is listed.
func TestWorkerPeerPlaceholderMatchesAResolvedPeerCopy(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: rdsTarget()})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource,
		&fakeResolver{err: errors.New("DescribeDBInstances: throttled")})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	res := cycle(t, ctx, wB)
	if res.TargetError == "" {
		t.Fatalf("B = %+v, want its target unresolved", res)
	}
	if res.Proposed != 0 || res.HeldByPeer != 1 {
		t.Fatalf("B with placeholders = %+v, want the change held by A", res)
	}
	if n := storedCount(t, poolB); n != 0 {
		t.Fatalf("B stored %d placeholder copies of A's change", n)
	}
}

// The converse stays apart: a resolved proposal is not held by a peer's
// copy for another target (the plan: different targets never merge).
func TestWorkerPeerResolvedTargetsMustMatch(t *testing.T) {
	poolA, poolB, ctx := peerPools(t)
	other := rdsTarget()
	other.ParameterGroup = "billing-pg16"
	wA, _ := peerWorker(t, poolA, "peer-a", peerResource, &fakeResolver{target: other})
	wB, _ := peerWorker(t, poolB, "peer-b", peerResource, &fakeResolver{target: rdsTarget()})
	insertIntentFinding(t, poolA, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, poolB, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	cycle(t, ctx, wA)
	if res := cycle(t, ctx, wB); res.Proposed != 1 || res.HeldByPeer != 0 {
		t.Fatalf("B on another parameter group = %+v, want 1 proposed", res)
	}
}
