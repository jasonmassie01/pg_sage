package earned

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Perf regression (2026-10-04): GET /api/v1/trust took 1.2-2.1 s under load
// while each of its statements executed in milliseconds. The view sent 17
// statements one after another; on a loaded host every round trip costs
// 10-350 ms (measured: SELECT 1 p50 11 ms, p99 152 ms, max 363 ms), so the
// Trust page paid the network 17 times. The ledger's reads now travel in
// one pipelined batch, and the effective levels reuse the family safety
// record the view already read instead of reading it again.

// trustViewMaxRoundTrips bounds the round trips of one annotated view:
// the proposal expiry write and one batch of reads.
const trustViewMaxRoundTrips = 2

func TestTrustViewTakesAConstantSmallNumberOfRoundTrips(t *testing.T) {
	f := newReconFixture(t)
	svc, lim, rec := tracedLedger(t, f)
	_, _ = annotatedView(t, svc, lim, rec)
	if n := rec.RoundTrips(); n > trustViewMaxRoundTrips {
		t.Fatalf("an empty view took %d round trips, want <= %d:\n%s", n,
			trustViewMaxRoundTrips, statementList(rec))
	}
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db,
		rampBound(f.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recordSpread(t, f)
	v, _ := annotatedView(t, svc, lim, rec)
	if n := rec.RoundTrips(); n > trustViewMaxRoundTrips {
		t.Fatalf("a populated view took %d round trips, want <= %d:\n%s", n,
			trustViewMaxRoundTrips, statementList(rec))
	}
	if len(v.Rows) < 30 || v.Grandfathered == nil {
		t.Fatalf("view lost content: %d rows, grandfathered %+v", len(v.Rows), v.Grandfathered)
	}
}

// The family safety record is read once per view: the effective levels
// reuse the evidence read's copy.
func TestTrustViewReadsTheSafetyRecordOnce(t *testing.T) {
	f := newReconFixture(t)
	svc, lim, rec := tracedLedger(t, f)
	recordSpread(t, f)
	v, _ := annotatedView(t, svc, lim, rec)
	if n := len(rec.Matching("result IN ('harmful', 'safety_violation')",
		"max(o.recorded_at)")); n != 1 {
		t.Fatalf("safety record read %d times, want once:\n%s", n, statementList(rec))
	}
	freeze, _ := trustRow(v, FamilyWraparound, ClassFreeze)
	if !hasDowngrade(freeze.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("reused safety record lost the wraparound signal: %+v", freeze.Downgrades)
	}
	cancel, _ := trustRow(v, FamilyLockBlocking, ClassBackendCancel)
	if hasDowngrade(cancel.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("lock_blocking took wraparound's signal: %+v", cancel.Downgrades)
	}
}

// AnnotateTrust on a view that did not come from TrustView (no safety
// record attached) still reads the record itself: the signal is never
// silently lost.
func TestAnnotateTrustReadsSafetyWhenTheViewCarriesNone(t *testing.T) {
	f := newReconFixture(t)
	svc, lim, rec := tracedLedger(t, f)
	recordSpread(t, f)
	v, err := svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	bare := TrustView{Database: v.Database, Rows: v.Rows}
	rec.Reset()
	lim.AnnotateTrust(f.ctx, &bare)
	if n := len(rec.Matching("result IN ('harmful', 'safety_violation')")); n != 1 {
		t.Fatalf("bare view: safety read %d times, want 1", n)
	}
	freeze, _ := trustRow(bare, FamilyWraparound, ClassFreeze)
	if !hasDowngrade(freeze.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("bare view lost the safety signal: %+v", freeze.Downgrades)
	}
}

// A failed batch is an error naming what failed, never a partial view.
func TestTrustViewBatchErrorIsReturned(t *testing.T) {
	f := newReconFixture(t)
	svc, _, _ := tracedLedger(t, f)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	v, err := svc.TrustView(ctx)
	if err == nil {
		t.Fatalf("cancelled view returned %d rows and no error", len(v.Rows))
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if len(v.Rows) != 0 {
		t.Fatalf("partial view with %d rows returned alongside the error", len(v.Rows))
	}
}
