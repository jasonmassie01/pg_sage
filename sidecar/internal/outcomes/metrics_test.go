package outcomes

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// pg_sage_outcomes_measured_ratio: recorded per database from a read of
// the default window with no filter, and only from a complete read.

func share(v float64) *float64 { return &v }

func TestObserve_RecordsOnlyTheDefaultUnfilteredWindow(t *testing.T) {
	resetMeasured()
	t.Cleanup(resetMeasured)
	def := Query{Since: DefaultSince, Limit: DefaultLimit}
	Observe("orders", def, Result{Summary: Summary{Changes: 3, Measured: 1,
		MeasuredShare: share(0.333)}})
	Observe("billing", Query{Since: 48 * time.Hour}, Result{Summary: Summary{
		MeasuredShare: share(1)}})
	Observe("billing", Query{Since: DefaultSince, Verdict: VerdictImproved},
		Result{Summary: Summary{MeasuredShare: share(1)}})
	Observe("billing", Query{Since: DefaultSince, Evidence: "ev-1"},
		Result{Summary: Summary{MeasuredShare: share(1)}})
	Observe("billing", def, Result{Truncated: true, Summary: Summary{MeasuredShare: share(1)}})
	Observe("billing", def, Result{Errors: []ReadError{{Database: "billing", Error: "x"}},
		Summary: Summary{MeasuredShare: share(1)}})
	got := MeasuredRatios()
	if len(got) != 1 || got["orders"] != 0.333 {
		t.Fatalf("ratios = %v, want only orders = 0.333", got)
	}
	Observe("orders", def, Result{Summary: Summary{}})
	if got := MeasuredRatios(); len(got) != 0 {
		t.Fatalf("a window with no changes leaves no sample, got %v", got)
	}
	Observe("", def, Result{Summary: Summary{MeasuredShare: share(1)}})
	if got := MeasuredRatios(); len(got) != 0 {
		t.Fatalf("an unnamed database is not recorded, got %v", got)
	}
}

func TestObserve_ConcurrentWritersAndReaders(t *testing.T) {
	resetMeasured()
	t.Cleanup(resetMeasured)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			Observe(fmt.Sprintf("db%d", i%4), Query{Since: DefaultSince},
				Result{Summary: Summary{Changes: 2, Measured: 1, MeasuredShare: share(0.5)}})
		}(i)
		go func() {
			defer wg.Done()
			for _, v := range MeasuredRatios() {
				if v != 0.5 {
					t.Errorf("ratio %v, want 0.5", v)
				}
			}
		}()
	}
	wg.Wait()
	if got := MeasuredRatios(); len(got) != 4 {
		t.Fatalf("ratios = %v, want four databases", got)
	}
}
