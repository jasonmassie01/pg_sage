package clone

import (
	"context"
	"testing"
	"time"
)

// lab-05a review: the reap claim re-checks the holder's heartbeat in the
// UPDATE that moves the receipt, a heartbeat refreshes only the holder's
// own live receipts, and a receipt without a holder or heartbeat (one
// written before ddlCloneInstancesHolder) is ended only by its TTL.

func reviewQuery(holder string) ReapQuery {
	return ReapQuery{Scope: reaperScope, Purpose: PurposeRehearsal, Holder: holder,
		StaleAfter: DefaultHolderStaleAfter}
}

// recordHeld writes a creating receipt held by holder, an hour from expiry.
func recordHeld(t *testing.T, s *ReceiptStore, name, holder string) Receipt {
	t.Helper()
	r, err := s.RecordLimited(context.Background(), Receipt{DeploymentID: testDeployment,
		Adapter: reaperScope.Adapter, Scope: reaperScope.Scope, Name: name,
		Purpose: PurposeRehearsal, HolderID: holder, ExpiresAt: time.Now().Add(time.Hour)}, 0)
	if err != nil {
		t.Fatalf("record %s: %v", name, err)
	}
	return r
}

func TestHolder_ClaimRechecksTheHeartbeatInTheSameStatement(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	ready := recordHeld(t, s, "rehearsal-recheck-ready", "holder-a")
	if _, err := s.MarkReady(ctx, ready.ID, "ref-ready"); err != nil {
		t.Fatal(err)
	}
	creating := recordHeld(t, s, "rehearsal-recheck-creating", "holder-a")
	ageHeartbeat(t, pool, ready.ID, time.Hour)
	ageHeartbeat(t, pool, creating.ID, time.Hour)
	q := reviewQuery("holder-b")
	due, err := s.Reapable(ctx, q)
	if err != nil || len(due) != 2 {
		t.Fatalf("reapable before the heartbeat: %d receipts, %v; want 2", len(due), err)
	}
	// The holder heartbeats between the reaper's read and its claim.
	if n, err := s.Heartbeat(ctx, "holder-a", []int64{ready.ID, creating.ID}); err != nil ||
		n != 2 {
		t.Fatalf("heartbeat refreshed %d, %v; want 2", n, err)
	}
	if _, ok, err := s.ClaimForReap(ctx, q, ready.ID); err != nil || ok {
		t.Fatalf("claim of a just-heartbeated ready receipt: ok %v, %v; want false", ok, err)
	}
	if _, ok, err := s.ResolveFound(ctx, q, creating.ID, "ref-found"); err != nil || ok {
		t.Fatalf("resolve of a just-heartbeated creating receipt: ok %v, %v; want false",
			ok, err)
	}
	if got := receiptByID(t, pool, ready.ID); got.Status != StatusReady {
		t.Fatalf("ready receipt is %s after a refused claim, want ready", got.Status)
	}
	if got := receiptByID(t, pool, creating.ID); got.Status != StatusCreating || got.Ref != "" {
		t.Fatalf("creating receipt is %s ref %q after a refused resolve", got.Status, got.Ref)
	}
}

func TestHolder_HeartbeatRefreshesOnlyTheHoldersOwnLiveReceipts(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	own := recordHeld(t, s, "rehearsal-beat-own", "holder-a")
	uncertain := recordHeld(t, s, "rehearsal-beat-uncertain", "holder-a")
	if _, err := s.MarkCreateFailed(ctx, uncertain.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{own.ID, uncertain.ID} {
		ageHeartbeat(t, pool, id, time.Hour)
	}
	before := heartbeatOf(t, pool, own.ID)
	if n, err := s.Heartbeat(ctx, "holder-b", []int64{own.ID}); err != nil || n != 0 {
		t.Fatalf("another holder refreshed %d receipts, %v; want 0", n, err)
	}
	if !heartbeatOf(t, pool, own.ID).Equal(before) {
		t.Fatal("another holder's heartbeat moved this holder's receipt")
	}
	if n, err := s.Heartbeat(ctx, "holder-a", []int64{own.ID, uncertain.ID}); err != nil ||
		n != 1 {
		t.Fatalf("own heartbeat refreshed %d, %v; want 1 (not the uncertain create)", n, err)
	}
	if !heartbeatOf(t, pool, own.ID).After(before.Add(50 * time.Minute)) {
		t.Fatal("the holder's own creating receipt was not refreshed")
	}
	// Nothing to say: no query, no error.
	if n, err := s.Heartbeat(ctx, "holder-a", nil); err != nil || n != 0 {
		t.Fatalf("empty heartbeat: %d, %v", n, err)
	}
	if n, err := s.Heartbeat(ctx, "", []int64{own.ID}); err != nil || n != 0 {
		t.Fatalf("heartbeat without a holder: %d, %v", n, err)
	}
}

func TestHolder_ReceiptWithoutHolderOrHeartbeatIsLeftToItsTTL(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := newHolderFake(KindBranch)
	// A receipt from before ddlCloneInstancesHolder: an uncertain create
	// with no holder.
	legacy := recordHeld(t, s, "rehearsal-legacy-uncertain", "")
	if _, err := s.MarkCreateFailed(ctx, legacy.ID, true); err != nil {
		t.Fatal(err)
	}
	// A holder whose heartbeat was never written.
	silent := recordHeld(t, s, "rehearsal-no-heartbeat", "holder-a")
	if _, err := s.MarkReady(ctx, silent.ID, "ref-silent"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances SET heartbeat_at = NULL
		WHERE id = $1`, silent.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := holderReaper(s, inner, "holder-b").Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Destroyed)+len(rep.Unresolved)+len(rep.Failed)+len(rep.FindFailed) != 0 ||
		inner.finds != 0 {
		t.Fatalf("a receipt without holder liveness was acted on before its TTL: %+v, "+
			"%d finds", rep, inner.finds)
	}
	if got := receiptByID(t, pool, legacy.ID); got.Status != StatusCreateUncertain {
		t.Fatalf("legacy receipt %s, want create_uncertain", got.Status)
	}
	if got := receiptByID(t, pool, silent.ID); got.Status != StatusReady {
		t.Fatalf("receipt without a heartbeat %s, want ready", got.Status)
	}
}
