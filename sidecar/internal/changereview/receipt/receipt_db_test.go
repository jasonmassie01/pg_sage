package receipt

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/verify"
)

// CHECK-RCP-03 and CHECK-RCP-04: once the window after landing closes,
// the watched statements are compared before and after with the Welch
// test over sage.query_store, and lock harm on the touched relations
// around landing is looked up. The receipt is decided once and sealed.

// landedReview seeds a concurred review of verdict that landed (reported)
// 25 hours ago, watching qid on rv.rv_orders.
func (f *fixture) landedReview(verdict string, qid int64) (changereview.Review, time.Time) {
	f.t.Helper()
	at := time.Now().Add(-25 * time.Hour).UTC().Truncate(time.Microsecond)
	r := f.seed(func(r *changereview.Review) {
		r.Verdict, r.DeterministicVerdict = verdict, verdict
		r.CreatedAt, r.CompletedAt = at.Add(-2*time.Hour), ptr(at.Add(-2*time.Hour+time.Minute))
		concurred(r)
		landedAt(r, changereview.LandingSourceReported, at, at)
		watching(r, []string{"rv.rv_orders"}, qid)
	})
	return r, at
}

func TestReceiptCleanWhenLatencyHolds(t *testing.T) {
	f := newFixture(t)
	r, at := f.landedReview(changereview.VerdictSafe, 501)
	f.beforeAfter(501, at, at, 10, 5, 5)
	rep := f.run()
	got := f.get(r.ID)
	if rep.Decided != 1 || got.ReceiptVerdict != changereview.ReceiptClean {
		t.Fatalf("receipt %s, report %+v; want clean", got.ReceiptVerdict, rep)
	}
	rc := got.Receipt
	if rc == nil || rc.Tolerance != verify.ToleranceMet || len(rc.Targets) != 1 ||
		rc.Targets[0].BeforeCalls < 30 || rc.Targets[0].AfterCalls < 30 {
		t.Fatalf("receipt %+v, want tolerance met with 30+ calls per side", rc)
	}
	if got.ReceiptHash == "" || !changereview.VerifyReceipt(got) ||
		got.ReceiptDecidedAt == nil {
		t.Fatal("the clean receipt is not sealed")
	}
	if got.CreditState != changereview.CreditFinal || got.CreditFinalAt == nil ||
		got.CreditedMinutes != 15 {
		t.Fatalf("credit %s/%d final_at %v, want final 15", got.CreditState,
			got.CreditedMinutes, got.CreditFinalAt)
	}
}

func TestReceiptCleanLowTrafficWithFewSamples(t *testing.T) {
	f := newFixture(t)
	r, at := f.landedReview(changereview.VerdictSafe, 502)
	f.beforeAfter(502, at, at, 1, 5, 5)
	f.run()
	got := f.get(r.ID)
	if got.ReceiptVerdict != changereview.ReceiptCleanLowTraffic ||
		got.Receipt.Reason != ReasonLowTraffic {
		t.Fatalf("receipt %s (%+v), want clean_low_traffic", got.ReceiptVerdict, got.Receipt)
	}
	if got.CreditState != changereview.CreditFinal {
		t.Fatalf("credit %s, want final for a low-traffic clean receipt", got.CreditState)
	}
}

// The window after landing has not closed yet: nothing is decided.
func TestReceiptWaitsForTheWindowToClose(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-23 * time.Hour).UTC().Truncate(time.Microsecond)
	r := f.seed(func(r *changereview.Review) {
		landedAt(r, changereview.LandingSourceReported, at, at)
	})
	if rep := f.run(); rep.Decided != 0 {
		t.Fatalf("decided before the window closed: %+v", rep)
	}
	if got := f.get(r.ID); got.ReceiptVerdict != changereview.ReceiptPending {
		t.Fatalf("receipt %s, want pending", got.ReceiptVerdict)
	}
}

func TestReceiptRegressedNamesTheQueryAndCountsFalseSafe(t *testing.T) {
	f := newFixture(t)
	since := time.Now().Add(-7 * 24 * time.Hour)
	before, err := f.svc.Summary(f.ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	r, at := f.landedReview(changereview.VerdictSafe, 503)
	f.beforeAfter(503, at, at, 10, 5, 9)
	other, at2 := f.landedReview(changereview.VerdictReview, 504)
	f.beforeAfter(504, at2, at2, 10, 5, 9)
	f.run()
	got := f.get(r.ID)
	if got.ReceiptVerdict != changereview.ReceiptRegressed ||
		got.Receipt.Reason != ReasonLatency {
		t.Fatalf("receipt %s (%+v), want regressed (latency)", got.ReceiptVerdict,
			got.Receipt)
	}
	if len(got.Receipt.ContradictedBy) != 1 || got.Receipt.ContradictedBy[0] != "queryid:503" {
		t.Fatalf("contradicted_by %v, want queryid:503", got.Receipt.ContradictedBy)
	}
	tg := got.Receipt.Targets[0]
	if tg.Verdict != verify.OutcomeRegressed || tg.ChangePct < 60 || tg.ChangePct > 90 {
		t.Fatalf("target %+v, want about +80%%", tg)
	}
	if got.CreditState != changereview.CreditVoid {
		t.Fatalf("credit %s, want void", got.CreditState)
	}
	if o := f.get(other.ID); o.ReceiptVerdict != changereview.ReceiptRegressed {
		t.Fatalf("second review receipt %s, want regressed", o.ReceiptVerdict)
	}
	after, err := f.svc.Summary(f.ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if after.FalseSafe != before.FalseSafe+1 {
		t.Fatalf("false_safe %d -> %d, want +1 (only the safe verdict counts)",
			before.FalseSafe, after.FalseSafe)
	}
	cal, err := f.svc.Store().Calibration(f.ctx, since)
	if err != nil || cal.FalseSafe != 1 {
		t.Fatalf("calibration false-safe %d (%v), want 1", cal.FalseSafe, err)
	}
}

var lockHarmCases = []struct {
	name  string
	harm  func(f *fixture, at time.Time)
	lock  bool
	check func(t *testing.T, rc *changereview.Receipt)
}{
	{"incident on the table", func(f *fixture, at time.Time) {
		f.incident(at.Add(5*time.Minute), "rv.rv_orders")
	}, true, func(t *testing.T, rc *changereview.Receipt) {
		if len(rc.Incidents) != 1 {
			t.Fatalf("incidents %v, want one", rc.Incidents)
		}
	}},
	{"lock_chain finding on the table", func(f *fixture, at time.Time) {
		f.lockFinding(at.Add(-10*time.Minute), "rv.rv_orders")
	}, true, func(t *testing.T, rc *changereview.Receipt) {
		if len(rc.LockFindings) != 1 {
			t.Fatalf("lock findings %v, want one", rc.LockFindings)
		}
	}},
	{"incident outside 15 minutes", func(f *fixture, at time.Time) {
		f.incident(at.Add(30*time.Minute), "rv.rv_orders")
	}, false, nil},
	{"lock_chain finding 30 minutes before landing", func(f *fixture, at time.Time) {
		f.lockFinding(at.Add(-30*time.Minute), "rv.rv_orders")
	}, false, nil},
	{"lock_chain finding on another table", func(f *fixture, at time.Time) {
		f.lockFinding(at, "rv.rv_items")
	}, false, nil},
}

func TestReceiptRegressedByLockHarm(t *testing.T) {
	for _, tc := range lockHarmCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r, at := f.landedReview(changereview.VerdictSafe, 505)
			f.beforeAfter(505, at, at, 10, 5, 5)
			tc.harm(f, at)
			f.run()
			got := f.get(r.ID)
			if !tc.lock {
				if got.ReceiptVerdict != changereview.ReceiptClean {
					t.Fatalf("receipt %s (%+v), want clean", got.ReceiptVerdict, got.Receipt)
				}
				return
			}
			if got.ReceiptVerdict != changereview.ReceiptRegressed ||
				got.Receipt.Reason != ReasonLock {
				t.Fatalf("receipt %s (%+v), want regressed (lock)", got.ReceiptVerdict,
					got.Receipt)
			}
			tc.check(t, got.Receipt)
			if got.CreditState != changereview.CreditVoid {
				t.Fatalf("credit %s, want void", got.CreditState)
			}
		})
	}
}

// An incident naming the table unqualified (public schema) matches.
func TestReceiptLockHarmMatchesPublicNames(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-25 * time.Hour).UTC().Truncate(time.Microsecond)
	r := f.seed(func(r *changereview.Review) {
		landedAt(r, changereview.LandingSourceReported, at, at)
		watching(r, []string{"public.orders"})
	})
	f.incident(at.Add(time.Minute), "orders")
	f.run()
	if got := f.get(r.ID); got.ReceiptVerdict != changereview.ReceiptRegressed {
		t.Fatalf("receipt %s, want regressed by the incident on orders", got.ReceiptVerdict)
	}
}
