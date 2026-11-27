package pgaudit

import (
	"sync"
	"testing"
)

func countOf(database, outcome string) int64 {
	for _, c := range RecordCounts() {
		if c.Database == database && c.Outcome == outcome {
			return c.Count
		}
	}
	return 0
}

// Happy path, zero/invalid input and ordering: counts add up per database
// and outcome, n of 0 or below counts nothing, the snapshot is sorted.
func TestCountRecords(t *testing.T) {
	CountRecords("metrics_b", OutcomeStored, 2)
	CountRecords("metrics_b", OutcomeStored, 3)
	CountRecords("metrics_b", OutcomeDropped, 0)
	CountRecords("metrics_b", OutcomeDropped, -4)
	CountRecords("metrics_a", OutcomeUnmatched, 1)
	if got := countOf("metrics_b", OutcomeStored); got != 5 {
		t.Fatalf("stored = %d, want 5", got)
	}
	for _, c := range RecordCounts() {
		if c.Database == "metrics_b" && c.Outcome == OutcomeDropped {
			t.Fatalf("a zero or negative count made a series: %+v", c)
		}
	}
	counts := RecordCounts()
	for i := 1; i < len(counts); i++ {
		a, b := counts[i-1], counts[i]
		if a.Database > b.Database || (a.Database == b.Database && a.Outcome >= b.Outcome) {
			t.Fatalf("snapshot not sorted at %d: %+v then %+v", i, a, b)
		}
	}
}

// Concurrent access: counts from many goroutines are all kept.
func TestCountRecordsConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				CountRecords("metrics_concurrent", OutcomeRequeued, 1)
				_ = RecordCounts()
			}
		}()
	}
	wg.Wait()
	if got := countOf("metrics_concurrent", OutcomeRequeued); got != 1600 {
		t.Fatalf("requeued = %d, want 1600", got)
	}
}
