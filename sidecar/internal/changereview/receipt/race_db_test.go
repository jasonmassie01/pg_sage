package receipt

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// CHECK-RCP-05: a statistics reset, restart or failover inside either
// window makes the comparison untrustworthy (unverifiable, confounded).
// Receipts are decided once: racing jobs decide each exactly once and
// later cycles never change a decided receipt.

func TestReceiptConfounders(t *testing.T) {
	cases := []struct {
		kind   changefeed.Kind
		offset time.Duration // from landing
		want   string
	}{
		{changefeed.KindStatsReset, -3 * time.Hour, changereview.ReceiptUnverifiable},
		{changefeed.KindRestart, 2 * time.Hour, changereview.ReceiptUnverifiable},
		{changefeed.KindFailover, 23 * time.Hour, changereview.ReceiptUnverifiable},
		{changefeed.KindRestart, -30 * time.Hour, changereview.ReceiptClean},
		{changefeed.KindExtension, time.Hour, changereview.ReceiptClean},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+tc.offset.String(), func(t *testing.T) {
			f := newFixture(t)
			r, at := f.landedReview(changereview.VerdictSafe, 601)
			f.beforeAfter(601, at, at, 10, 5, 5)
			f.event(tc.kind, "", at.Add(tc.offset))
			f.run()
			got := f.get(r.ID)
			if got.ReceiptVerdict != tc.want {
				t.Fatalf("receipt %s (%+v), want %s", got.ReceiptVerdict, got.Receipt, tc.want)
			}
			if tc.want == changereview.ReceiptUnverifiable &&
				(got.Receipt.Reason != ReasonConfounded || len(got.Receipt.Confounders) != 1) {
				t.Fatalf("receipt %+v, want reason confounded naming the event", got.Receipt)
			}
			if tc.want == changereview.ReceiptUnverifiable &&
				got.CreditState != changereview.CreditVoid {
				t.Fatalf("credit %s, want void for an unverifiable receipt", got.CreditState)
			}
		})
	}
}

func TestReceiptRacingJobsDecideOnce(t *testing.T) {
	f := newFixture(t)
	const n = 20
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		r, at := f.landedReview(changereview.VerdictSafe, int64(700+i))
		f.beforeAfter(int64(700+i), at, at, 10, 5, 5)
		ids = append(ids, r.ID)
	}
	reports := f.raceJobs(2)
	if d := reports[0].Decided + reports[1].Decided; d != n {
		t.Fatalf("decided %d + %d, want exactly %d", reports[0].Decided,
			reports[1].Decided, n)
	}
	sealed := map[string]changereview.Review{}
	for _, id := range ids {
		got := f.get(id)
		if got.ReceiptVerdict != changereview.ReceiptClean || !changereview.VerifyReceipt(got) {
			t.Fatalf("review %s: receipt %s sealed %v", id, got.ReceiptVerdict,
				changereview.VerifyReceipt(got))
		}
		sealed[id] = got
	}
	f.clock.advance(time.Hour)
	if rep := f.run(); rep.Decided != 0 {
		t.Fatalf("a later cycle decided again: %+v", rep)
	}
	for _, id := range ids {
		got := f.get(id)
		if got.ReceiptHash != sealed[id].ReceiptHash ||
			!got.ReceiptDecidedAt.Equal(*sealed[id].ReceiptDecidedAt) {
			t.Fatalf("review %s receipt changed after it was decided", id)
		}
	}
}

// raceJobs runs n jobs on the fixture's database at once.
func (f *fixture) raceJobs(n int) []Report {
	f.t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	reports := make([]Report, n)
	errs := make([]error, n)
	for i := range reports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			reports[i], errs[i] = f.job().RunDatabase(f.ctx, f.db())
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			f.t.Fatalf("job %d: %v", i, err)
		}
	}
	return reports
}
