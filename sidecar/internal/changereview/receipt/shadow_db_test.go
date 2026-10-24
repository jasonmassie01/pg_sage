package receipt

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// CHECK-RCP-08 scoring: the job scores the would-auto-clear flag once the
// DBA verdict and the receipt settle it, exactly once; the summary shows
// the evidence against the readiness bar (30 scored, 97% correct, no
// regressions in 90 days), labeled shadow only.

// flagged seeds an eligible review with a DBA verdict and receipt.
func (f *fixture) flagged(dba, receipt string) changereview.Review {
	f.t.Helper()
	return f.seed(func(r *changereview.Review) {
		r.ShadowAutoClear = ptr(true)
		decidedFacts(r, dba, receipt)
	})
}

// decidedFacts sets a DBA verdict and a receipt verdict consistently with
// the table's constraints (a decided receipt has landed).
func decidedFacts(r *changereview.Review, dba, receipt string) {
	at := r.CompletedAt.Add(time.Minute)
	if dba != "" {
		r.DBAVerdict, r.DBAActor, r.DBADecidedAt = dba, "user:1", &at
		if dba == changereview.DBAOverride {
			r.DBAReason, r.DBADirection = "fine as is", changereview.DirectionTooStrict
		}
	}
	if receipt != changereview.ReceiptPending {
		landedAt(r, changereview.LandingSourceReported, at, at)
		r.ReceiptVerdict, r.ReceiptDecidedAt = receipt, &at
		r.Receipt = &changereview.Receipt{Verdict: receipt}
	}
}

func TestShadowScoringOnceSettled(t *testing.T) {
	f := newFixture(t)
	correct := f.flagged(changereview.DBAConcur, changereview.ReceiptClean)
	override := f.flagged(changereview.DBAOverride, changereview.ReceiptPending)
	edits := f.flagged(changereview.DBAConcurWithEdits, changereview.ReceiptPending)
	regressed := f.flagged("", changereview.ReceiptRegressed)
	pending := f.flagged(changereview.DBAConcur, changereview.ReceiptPending)
	unflagged := f.seed(func(r *changereview.Review) {
		r.ShadowAutoClear = ptr(false)
		decidedFacts(r, changereview.DBAConcur, changereview.ReceiptClean)
	})
	rep := f.run()
	if rep.Scored != 4 {
		t.Fatalf("scored %d, want 4", rep.Scored)
	}
	want := map[string]string{correct.ID: changereview.ShadowCorrect,
		override.ID: changereview.ShadowIncorrect, edits.ID: changereview.ShadowIncorrect,
		regressed.ID: changereview.ShadowIncorrect, pending.ID: changereview.ShadowUnscored,
		unflagged.ID: changereview.ShadowUnscored}
	for id, score := range want {
		if got := f.get(id); got.ShadowScore != score {
			t.Errorf("review %s: shadow score %s, want %s", id, got.ShadowScore, score)
		}
	}
	if rep = f.run(); rep.Scored != 0 {
		t.Fatalf("a second cycle scored %d again", rep.Scored)
	}
	s, err := f.svc.Summary(f.ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a := s.AutoClear
	if a.Eligible != 5 || a.Scored != 4 || a.Correct != 1 || a.Regressions90d != 1 ||
		a.CorrectRate == nil || *a.CorrectRate != 0.25 || a.Ready {
		t.Fatalf("auto-clear readiness %+v", a)
	}
	if a.MinScored != 30 || a.MinCorrectRate != 0.97 || a.MaxRegressions != 0 ||
		a.Label != changereview.AutoClearLabel {
		t.Fatalf("readiness bar %+v, want 30 / 0.97 / 0, shadow only", a)
	}
}

// Thirty correct scores reach the bar; one regression drops it again.
func TestShadowReadinessBar(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 30; i++ {
		f.flagged(changereview.DBAConcur, changereview.ReceiptClean)
	}
	if rep := f.run(); rep.Scored != 30 {
		t.Fatalf("scored %d, want 30", rep.Scored)
	}
	since := time.Now().Add(-24 * time.Hour)
	s, err := f.svc.Summary(f.ctx, since)
	if err != nil || !s.AutoClear.Ready {
		t.Fatalf("readiness %+v (%v), want ready at 30 of 30", s.AutoClear, err)
	}
	f.flagged("", changereview.ReceiptRegressed)
	f.run()
	if s, err = f.svc.Summary(f.ctx, since); err != nil || s.AutoClear.Ready {
		t.Fatalf("readiness %+v (%v), want not ready after a regression", s.AutoClear, err)
	}
}
