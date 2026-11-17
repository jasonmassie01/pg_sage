package clone

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

// lab-05a: providers name their resources from the receipt, so a create
// that may or may not have happened (create_uncertain) or a creating
// receipt whose holder died is resolved by name and reaped. A provider
// that cannot look a resource up keeps "report, never guess".

var resourceNamePattern = regexp.MustCompile(`^sage-[a-z-]+-[0-9a-f]{8}$`)

func TestFinder_ResourceNameIsDeterministic(t *testing.T) {
	r := Receipt{Name: "rehearsal-1a2b3c4d-0000-4000-8000-000000000001",
		Purpose: PurposeRehearsal}
	if got := ResourceName(r); got != "sage-rehearsal-1a2b3c4d" || ResourceName(r) != got {
		t.Fatalf("ResourceName = %q, want sage-rehearsal-1a2b3c4d", got)
	}
	masked := Receipt{Name: "masked_parent-deadbeef-0000-4000-8000-000000000001",
		Purpose: PurposeMaskedParent}
	if got := ResourceName(masked); got != "sage-masked-parent-deadbeef" {
		t.Fatalf("masked_parent name %q", got)
	}
	// A receipt name not made by receiptName still yields a valid,
	// stable resource name (hashed), never an empty one.
	odd := Receipt{Name: "Hand Written", Purpose: PurposeSandbox}
	got := ResourceName(odd)
	if !resourceNamePattern.MatchString(got) || got != ResourceName(odd) {
		t.Fatalf("odd receipt name gave %q", got)
	}
	if ResourceName(Receipt{Name: "Other", Purpose: PurposeSandbox}) == got {
		t.Fatal("two receipt names gave one resource name")
	}
}

func TestFinder_CreateIsAskedForTheReceiptsResourceName(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	c, err := holderReceipting(t, inner, s, holderConfig("holder-a", ctx)).Create(ctx,
		CloneSpec{})
	if err != nil {
		t.Fatal(err)
	}
	r := onlyReceipt(t, pool)
	name := inner.resources[c.ID]
	if name != ResourceName(r) || !resourceNamePattern.MatchString(name) {
		t.Fatalf("provider named the clone %q, receipt %s wants %q", name, r.Name,
			ResourceName(r))
	}
}

func TestFinder_UncertainCreateIsResolvedByNameAndDestroyed(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	inner.createErr = context.DeadlineExceeded // the resource exists; the reply was lost
	life, kill := context.WithCancel(ctx)
	defer kill()
	p := holderReceipting(t, inner, s, holderConfig("holder-a", life))
	if _, err := p.Create(ctx, CloneSpec{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create: %v", err)
	}
	r := onlyReceipt(t, pool)
	if r.Status != StatusCreateUncertain || r.Ref != "" || inner.count() != 1 {
		t.Fatalf("after the lost reply: %s ref %q, %d resources", r.Status, r.Ref,
			inner.count())
	}
	// No one holds an uncertain create, so even this process resolves it
	// once its heartbeat is stale.
	ageHeartbeat(t, pool, r.ID, DefaultHolderStaleAfter+time.Second)
	rep, err := holderReaper(s, inner, "holder-a").Reap(ctx)
	if err != nil || len(rep.Destroyed) != 1 || inner.count() != 0 {
		t.Fatalf("resolve by name: %+v, %v; %d resources left", rep, err, inner.count())
	}
	got := receiptByID(t, pool, r.ID)
	if got.Status != StatusDestroyed || got.Ref != "ref-1" {
		t.Fatalf("receipt %s ref %q, want destroyed naming ref-1", got.Status, got.Ref)
	}
}

func TestFinder_AbandonedCreatingReceiptIsResolvedByName(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	// The process recorded, the provider made the clone, then the process
	// died before marking the receipt ready.
	r, err := s.RecordLimited(ctx, Receipt{DeploymentID: testDeployment,
		Adapter: reaperScope.Adapter, Scope: reaperScope.Scope,
		Name: "rehearsal-0badc0de-0000-4000-8000-000000000001", Purpose: PurposeRehearsal,
		HolderID: "holder-a", ExpiresAt: time.Now().Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inner.Create(ctx, CloneSpec{ResourceName: ResourceName(r)}); err != nil {
		t.Fatal(err)
	}
	ageHeartbeat(t, pool, r.ID, time.Hour)
	rep, err := holderReaper(s, inner, "holder-b").Reap(ctx)
	if err != nil || len(rep.Destroyed) != 1 || inner.count() != 0 {
		t.Fatalf("abandoned creating receipt: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroyed {
		t.Fatalf("receipt %s, want destroyed", got.Status)
	}
}

func TestFinder_LookupErrorLeavesTheReceiptUnresolved(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	inner.createErr = context.Canceled
	life, kill := context.WithCancel(ctx)
	defer kill()
	if _, err := holderReceipting(t, inner, s, holderConfig("holder-a", life)).Create(ctx,
		CloneSpec{}); err == nil {
		t.Fatal("create succeeded")
	}
	r := onlyReceipt(t, pool)
	ageHeartbeat(t, pool, r.ID, time.Hour)
	boom := errors.New("lookup 500")
	inner.findErr = boom
	rep, err := holderReaper(s, inner, "holder-b").Reap(ctx)
	if err != nil || len(rep.Unresolved) != 1 || !errors.Is(rep.FindFailed[r.ID], boom) {
		t.Fatalf("lookup error: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusCreateUncertain ||
		inner.count() != 1 {
		t.Fatalf("receipt %s with %d resources; a failed lookup changed something",
			got.Status, inner.count())
	}
}

func TestFinder_NotFoundWaitsForTheTTLThenCloses(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	inner.failCreate = context.DeadlineExceeded // nothing was made after all
	life, kill := context.WithCancel(ctx)
	defer kill()
	if _, err := holderReceipting(t, inner, s, holderConfig("holder-a", life)).Create(ctx,
		CloneSpec{}); err == nil {
		t.Fatal("create succeeded")
	}
	r := onlyReceipt(t, pool)
	ageHeartbeat(t, pool, r.ID, time.Hour)
	reaper := holderReaper(s, inner, "holder-b")
	rep, err := reaper.Reap(ctx)
	if err != nil || len(rep.Unresolved) != 1 || len(rep.Destroyed) != 0 {
		t.Fatalf("not found before the TTL: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusCreateUncertain {
		t.Fatalf("receipt %s before the TTL, want create_uncertain (it may still land)",
			got.Status)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances SET expires_at =
		now() - interval '1 minute' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if rep, err = reaper.Reap(ctx); err != nil || len(rep.Unresolved) != 0 {
		t.Fatalf("not found after the TTL: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusFailed {
		t.Fatalf("receipt %s after the TTL, want failed (nothing exists)", got.Status)
	}
}

func TestFinder_WithoutFinderReportsAndNeverGuesses(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := &fakeInner{pool: pool, createErr: context.DeadlineExceeded}
	cfg := holderConfig("holder-a", ctx)
	if _, err := holderReceipting(t, inner, s, cfg).Create(ctx,
		CloneSpec{}); err == nil {
		t.Fatal("create succeeded")
	}
	r := onlyReceipt(t, pool)
	ageHeartbeat(t, pool, r.ID, time.Hour)
	rep, err := holderReaper(s, inner, "holder-b").Reap(ctx)
	if err != nil || len(rep.Unresolved) != 1 || len(rep.FindFailed) != 0 {
		t.Fatalf("no finder: %+v, %v", rep, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusCreateUncertain ||
		len(inner.statusAtDestroy) != 0 {
		t.Fatalf("receipt %s, %d destroys: a provider without Finder guessed", got.Status,
			len(inner.statusAtDestroy))
	}
}
