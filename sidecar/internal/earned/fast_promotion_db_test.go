package earned

import (
	"errors"
	"testing"
	"time"
)

// Fast elevation end to end on PostgreSQL: with the fastest promotion bar
// a pair earns L2 and then L3 within hours of real reviews and recoveries,
// but only an admin's approval changes a level, irreversible and
// mitigation-only classes stay under their caps, L4 is never proposed and
// a harmful outcome still demotes the family.

func newFastFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	cfg := DefaultConfig()
	cfg.Now, cfg.EvidenceCacheTTL = f.clock.Now, 0
	cfg.Thresholds = fastestThresholds()
	svc, err := NewService(f.store, cfg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	f.svc = svc
	return f
}

// seedFastShadow records four accepted reviews: the first 61 minutes ago
// (the shadow has run for over an hour) and three inside the 1-hour
// window (the volume is counted inside the window).
func (f *fixture) seedFastShadow(family Family) {
	f.t.Helper()
	now := f.clock.Now()
	for i, at := range []time.Duration{61 * time.Minute, 50 * time.Minute,
		30 * time.Minute, time.Minute} {
		f.clock.Set(now.Add(-at))
		if err := f.svc.RecordReview(f.ctx, Review{Database: f.db,
			InvestigationID: newUUID(f.t), Family: family, Verdict: VerdictAccepted,
			Reviewer: "user:7:ops@example.com"}); err != nil {
			f.t.Fatalf("review %d: %v", i, err)
		}
	}
	f.clock.Set(now)
	if _, err := f.svc.IngestEvalRun(f.ctx, benchReport(now.Add(-time.Hour), family),
		SourceBench, "user:1:admin@example.com", ""); err != nil {
		f.t.Fatalf("ingest bench: %v", err)
	}
}

func TestFastBarElevatesInHoursWithAdminApproval(t *testing.T) {
	f := newFastFixture(t)
	f.seedFastShadow(FamilyWraparound)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWraparound, ClassFreeze)
	if p == nil || p.To != L2 {
		t.Fatalf("no L2 proposal after a 61-minute shadow under the fast bar: %+v", p)
	}
	for _, actor := range []string{ActorPgSage, "system", "mcp:agent-1"} {
		if _, err := f.svc.Approve(f.ctx, p.ID, actor, ""); !errors.Is(err,
			ErrHumanApprovalRequired) {
			t.Fatalf("%s approved a fast promotion: %v", actor, err)
		}
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L1 {
		t.Fatalf("level changed without an approval: %v", got)
	}
	if st := f.promote(FamilyWraparound, ClassFreeze); st.Level != L2 {
		t.Fatalf("approved level = %v, want L2", st.Level)
	}
	f.seedL2Recoveries(FamilyWraparound, ClassFreeze, 1)
	if st := f.promote(FamilyWraparound, ClassFreeze); st.Level != L3 {
		t.Fatalf("approved level = %v, want L3", st.Level)
	}
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range created {
		if p.Family == FamilyWraparound && p.Class == ClassFreeze {
			t.Fatalf("proposed above L3: %+v", p)
		}
	}
}

func TestFastBarNeverLiftsTheCaps(t *testing.T) {
	f := newFastFixture(t)
	f.seedFastShadow(FamilyWAL)
	f.seedL2Recoveries(FamilyWAL, ClassWALBound, 5)
	for i := 0; i < 5; i++ { // distinct action ids: evidence that would support L3
		if err := f.svc.RecordOutcome(f.ctx, Outcome{Database: f.db,
			ActionLogID: int64(200000 + i), Family: FamilyWAL, Class: ClassSlotDrop,
			Level: L2, Result: ResultVerifiedRecovery, Source: SourceExecutor,
			Actor: "pg_sage"}); err != nil {
			t.Fatalf("outcome: %v", err)
		}
	}
	if got := SupportedLevel(fastestThresholds(), mustEvidence(t, f, FamilyWAL,
		ClassSlotDrop)); got != L3 {
		t.Fatalf("fixture: slot_drop evidence supports %v, want L3 (capped below)", got)
	}
	for round := 0; round < 4; round++ {
		created, err := f.svc.ProposePromotions(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range created {
			if p.To > CapFor(p.Class) || p.To >= L4 {
				t.Fatalf("round %d proposed %s/%s to %v above its cap %v", round, p.Family,
					p.Class, p.To, CapFor(p.Class))
			}
			if _, err := f.svc.Approve(f.ctx, p.ID, "user:1:admin@example.com", ""); err != nil {
				t.Fatalf("approve %s/%s: %v", p.Family, p.Class, err)
			}
		}
	}
	if got := f.granted(FamilyWAL, ClassSlotDrop); got != L1 {
		t.Fatalf("irreversible slot_drop at %v under the fast bar, want L1", got)
	}
	if got := f.granted(FamilyWAL, ClassWALBound); got != L2 {
		t.Fatalf("mitigation-only wal_bound at %v under the fast bar, want L2", got)
	}
}

func TestFastBarStillDemotesOnHarm(t *testing.T) {
	f := newFastFixture(t)
	f.seedFastShadow(FamilyWraparound)
	f.promote(FamilyWraparound, ClassFreeze)
	f.seedL2Recoveries(FamilyWraparound, ClassFreeze, 1)
	f.promote(FamilyWraparound, ClassFreeze)
	err := f.svc.RecordOutcome(f.ctx, Outcome{Database: f.db, ActionLogID: 900001,
		Family: FamilyWraparound, Class: ClassFreeze, Level: L3, Result: ResultHarmful,
		Source: SourceOperator, Actor: "user:2:o@e", Detail: "freeze stalled writes"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L1 {
		t.Fatalf("level after harm under the fast bar = %v, want L1", got)
	}
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range created {
		if p.Family == FamilyWraparound {
			t.Fatalf("re-promotion inside the safety window under the fast bar: %+v", p)
		}
	}
}

// A service configured with an all-zero bar gets the spec bar, not "no bar".
func TestZeroThresholdsTakeTheSpecBar(t *testing.T) {
	f := newFixture(t)
	cfg := DefaultConfig()
	cfg.Now, cfg.EvidenceCacheTTL = f.clock.Now, 0
	// Partial: a fast shadow and the spec report age; every promotion
	// threshold left at zero must take the spec value, not pass.
	cfg.Thresholds = Thresholds{ShadowDuration: time.Hour,
		BenchMaxAge: DefaultThresholds().BenchMaxAge}
	svc, err := NewService(f.store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	f.seedFastShadow(FamilyWraparound)
	f.seedL2Recoveries(FamilyWraparound, ClassFreeze, 1)
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("3 reviews met a partial bar whose zero fields were skipped: %+v", created)
	}
}

func mustEvidence(t *testing.T, f *fixture, family Family, class ActionClass) Evidence {
	t.Helper()
	ev, err := f.svc.Evidence(f.ctx, family, class)
	if err != nil {
		t.Fatalf("evidence %s/%s: %v", family, class, err)
	}
	return ev
}
