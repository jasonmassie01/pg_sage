package earned

import (
	"context"
	"testing"
)

// Phase 1.1: "Evaluate now" explains itself. Evaluate proposes what the
// evidence supports and, for every applicable pair it did not propose,
// says why: a proposal already waits, the pair is at its cap, or the
// listed checks are unmet (each with its instruction).

func notProposed(e Evaluation, f Family, c ActionClass) *NotProposed {
	for i := range e.NotProposed {
		if e.NotProposed[i].Family == f && e.NotProposed[i].Class == c {
			return &e.NotProposed[i]
		}
	}
	return nil
}

func applicablePairs() int {
	n := 0
	for _, f := range Families() {
		n += len(ApplicableClasses(f))
	}
	return n
}

func TestEvaluateExplainsEveryPairItDidNotPropose(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	e, err := f.svc.Evaluate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Created) == 0 || len(e.Created)+len(e.NotProposed) != applicablePairs() {
		t.Fatalf("created %d + not proposed %d != %d applicable pairs", len(e.Created),
			len(e.NotProposed), applicablePairs())
	}
	lock := notProposed(e, FamilyLockBlocking, ClassBackendCancel)
	if lock == nil || lock.Reason != NotProposedEvidence || lock.Target == nil ||
		*lock.Target != L2 || lock.Granted != L1 || len(lock.Unmet) == 0 {
		t.Fatalf("lock_blocking/backend_cancel = %+v", lock)
	}
	for _, c := range lock.Unmet {
		if c.Met || c.How == "" {
			t.Fatalf("an unmet entry is met or has no instruction: %+v", c)
		}
	}
	slot := notProposed(e, FamilyWAL, ClassSlotDrop)
	if slot == nil || slot.Reason != NotProposedAtCap || slot.Target != nil ||
		len(slot.Unmet) != 0 {
		t.Fatalf("irreversible slot_drop at its L1 cap = %+v", slot)
	}
	again, err := f.svc.Evaluate(f.ctx)
	if err != nil || len(again.Created) != 0 {
		t.Fatalf("second evaluation = %+v (%v)", again.Created, err)
	}
	if wal := notProposed(again, FamilyWAL, ClassWALBound); wal == nil ||
		wal.Reason != NotProposedPending || wal.Pending == "" {
		t.Fatalf("a pair with a pending proposal = %+v", wal)
	}
}

func TestProposePromotionsReturnsTheCreatedProposals(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil || len(created) == 0 {
		t.Fatalf("created = %+v (%v)", created, err)
	}
	for _, p := range created {
		if p.Family != FamilyWAL || p.To != L2 {
			t.Fatalf("unexpected proposal %+v", p)
		}
	}
}

func TestEvaluateOnAFreshLedgerProposesNothing(t *testing.T) {
	f := newFixture(t)
	e, err := f.svc.Evaluate(f.ctx)
	if err != nil || len(e.Created) != 0 || e.Created == nil ||
		len(e.NotProposed) != applicablePairs() {
		t.Fatalf("fresh evaluation = %+v (%v)", e, err)
	}
}

func TestEvaluateStoreFailureIsAnError(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.svc.Evaluate(ctx); err == nil {
		t.Fatal("a cancelled evaluation must fail")
	}
}
