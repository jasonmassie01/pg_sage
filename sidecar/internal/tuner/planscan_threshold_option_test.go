package tuner

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// tuning-09: WithNestedLoopMinRows carries tuner.nested_loop_row_threshold
// into ScanPlan; a value <= 0 falls back to the 10,000-row default.

func TestNestedLoopMinRows_ZeroAndNegativeUseDefault(t *testing.T) {
	for _, threshold := range []int64{0, -5} {
		below := nestedLoopPlanJSON(t, 100, 2_000, 100, 100,
			nlInner{rows: ptr(9_999.0), loops: 1})
		if got := nestedLoopSymptoms(t, below, WithNestedLoopMinRows(threshold)); len(got) != 0 {
			t.Fatalf("threshold %d, 9999 inner rows fired: %+v", threshold, got)
		}
		at := nestedLoopPlanJSON(t, 100, 2_000, 100, 100,
			nlInner{rows: ptr(10_000.0), loops: 1})
		if got := nestedLoopSymptoms(t, at, WithNestedLoopMinRows(threshold)); len(got) != 1 {
			t.Fatalf("threshold %d, 10000 inner rows: got %d symptoms, want 1",
				threshold, len(got))
		}
	}
}

// A configured threshold replaces the default in both directions.
func TestNestedLoopMinRows_ConfiguredThreshold(t *testing.T) {
	cases := []struct {
		threshold int64
		innerRows float64
		want      int
	}{
		{100, 99, 0}, {100, 100, 1}, {100, 150, 1},
		{50_000, 49_999, 0}, {50_000, 20_000, 0}, {50_000, 50_000, 1},
	}
	for _, tc := range cases {
		plan := nestedLoopPlanJSON(t, 10, 1_000, 10, 1_000,
			nlInner{rows: ptr(tc.innerRows), loops: 1})
		got := nestedLoopSymptoms(t, plan, WithNestedLoopMinRows(tc.threshold))
		if len(got) != tc.want {
			t.Errorf("threshold %d, inner %.0f: got %d symptoms, want %d",
				tc.threshold, tc.innerRows, len(got), tc.want)
		}
	}
}

// The option composes with WithCatalogFacts in either order.
func TestNestedLoopMinRows_ComposesWithCatalogFacts(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 10, 1_000, 10, 1_000, nlInner{rows: ptr(500.0), loops: 1})
	a := nestedLoopSymptoms(t, plan, WithCatalogFacts(nil, 0), WithNestedLoopMinRows(400))
	b := nestedLoopSymptoms(t, plan, WithNestedLoopMinRows(400), WithCatalogFacts(nil, 0))
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("option order changed the result: %d vs %d symptoms, want 1 and 1",
			len(a), len(b))
	}
}

// Concurrent access: each ScanPlan call has its own scan context, so
// concurrent scans with different thresholds never see each other's value.
func TestNestedLoopMinRows_ConcurrentScansIsolated(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 10, 1_000, 10, 1_000, nlInner{rows: ptr(500.0), loops: 1})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			threshold, want := int64(400), 1
			if i%2 == 1 {
				threshold, want = 600, 0
			}
			symptoms, err := ScanPlan(plan, WithNestedLoopMinRows(threshold))
			if err != nil {
				errs <- err
				return
			}
			if got := countKind(symptoms, SymptomBadNestedLoop); got != want {
				errs <- fmt.Errorf("threshold %d: %d symptoms, want %d", threshold, got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// The tuner passes its configured threshold to ScanPlan: a 500-row inner
// side fires at threshold 400 and not at the default.
func TestNestedLoopMinRows_TunerUsesConfiguredThreshold(t *testing.T) {
	plan := nestedLoopPlanJSON(t, 10, 1_000, 10, 1_000, nlInner{rows: ptr(500.0), loops: 1})
	for _, tc := range []struct {
		threshold int64
		want      int
	}{{400, 1}, {0, 0}} {
		tu := New(nil, TunerConfig{NestedLoopRowThreshold: tc.threshold}, nil, noopLog)
		symptoms, err := tu.scanPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("threshold %d: scanPlan: %v", tc.threshold, err)
		}
		if got := countKind(symptoms, SymptomBadNestedLoop); got != tc.want {
			t.Errorf("threshold %d: %d symptoms, want %d", tc.threshold, got, tc.want)
		}
	}
	if _, err := New(nil, TunerConfig{}, nil, noopLog).scanPlan(
		context.Background(), []byte("not json")); err == nil {
		t.Fatal("scanPlan on invalid JSON: want an error")
	}
}

func countKind(symptoms []PlanSymptom, kind SymptomKind) int {
	n := 0
	for _, s := range symptoms {
		if s.Kind == kind {
			n++
		}
	}
	return n
}
