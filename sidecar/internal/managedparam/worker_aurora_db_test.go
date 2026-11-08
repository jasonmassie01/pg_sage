package managedparam

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Integration: an Aurora database reached through its writer's instance
// endpoint resolves to an aurora target (what cloudtel.AWSSource now
// returns), so the Aurora intent becomes one pending proposal on real
// PostgreSQL instead of a build error every cycle.
func TestWorkerAuroraInstanceEndpointWritesOnePendingProposal(t *testing.T) {
	pool, ctx := testPool(t)
	target := rdsTarget()
	target.Provider, target.InstanceID, target.ParameterGroup = "aurora", "writer-1",
		"shop-writer-pg16"
	target.Cluster, target.InstanceRole = "shop", InstanceRoleWriter
	w, err := NewWorker(WorkerOptions{Database: "shop", Provider: "aurora", Pool: pool,
		Store: NewStore(pool), Resolver: &fakeResolver{target: target},
		TargetTTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	finding := insertIntentFinding(t, pool, "instance:shared_buffers",
		intent(t, "aurora", "shared_buffers", "8GB"))
	res, err := w.Cycle(ctx)
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if res.Proposed != 1 || len(res.BuildErrors) != 0 || res.TargetError != "" {
		t.Fatalf("cycle = %+v, want one proposal and no errors", res)
	}
	rec := onlyRecord(t, w.store)
	if rec.Status != StatusPending || rec.FindingID == nil || *rec.FindingID != finding ||
		rec.Proposal.Provider != "aurora" || rec.Proposal.Target != "shop-writer-pg16" ||
		!strings.Contains(strings.Join(rec.Proposal.Notes, " "), "cluster shop") {
		t.Fatalf("record = %+v", rec)
	}
	if got := storedTarget(t, pool, rec.ID); got != "shop-writer-pg16" {
		t.Fatalf("stored target = %q", got)
	}
}

// Error propagation: an identity mismatch from the resolver is one target
// error naming the instance; the Aurora intent is still proposed (with
// placeholders) rather than failing to build.
func TestWorkerAuroraIdentityMismatchIsOneTargetError(t *testing.T) {
	pool, ctx := testPool(t)
	mismatch := fmt.Errorf("unavailable: provider resource identity: PostgreSQL " +
		"reports aurora but RDS instance writer-1 runs engine \"postgres\"")
	w, err := NewWorker(WorkerOptions{Database: "shop", Provider: "aurora", Pool: pool,
		Store: NewStore(pool), Resolver: &fakeResolver{err: mismatch}})
	if err != nil {
		t.Fatal(err)
	}
	insertIntentFinding(t, pool, "instance:shared_buffers",
		intent(t, "aurora", "shared_buffers", "8GB"))
	insertIntentFinding(t, pool, "instance:max_connections",
		intent(t, "aurora", "max_connections", "400"))
	res, err := w.Cycle(ctx)
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if !strings.Contains(res.TargetError, "writer-1") || len(res.BuildErrors) != 0 ||
		res.Proposed != 2 {
		t.Fatalf("cycle = %+v, want one target error naming writer-1", res)
	}
}
