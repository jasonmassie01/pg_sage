package executor

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// observability-07: the per-database counters are pruned when a database
// leaves the process, so /metrics stops exporting its series.

// countsFor is every park, release and record-failure count of database.
func countsFor(database string) (parks, releases, failures int) {
	for _, c := range ParkCounts() {
		if c.Database == database {
			parks++
		}
	}
	for _, c := range WaitReleaseCounts() {
		if c.Database == database {
			releases++
		}
	}
	for _, c := range RecordFailureCounts() {
		if c.Database == database {
			failures++
		}
	}
	return parks, releases, failures
}

func seedPruneCounters(database string) {
	countDecision(database, policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonRateLimitExceeded})
	countDecision(database, policy.Decision{Verdict: policy.VerdictExecute,
		VerificationWait: &policy.VerificationWait{Overridden: true,
			Pending: []policy.PendingVerification{waitFor(1)}}})
	countRecordFailure(database, "ledger_unavailable")
}

// Happy path and state transition: pruning one database removes all three
// counter kinds for it and leaves another database's counts untouched.
func TestPruneCountsDropsOnlyThatDatabase(t *testing.T) {
	stamp := time.Now().UnixNano()
	gone, kept := fmt.Sprintf("prune_gone_%d", stamp), fmt.Sprintf("prune_kept_%d", stamp)
	seedPruneCounters(gone)
	seedPruneCounters(kept)
	if p, r, f := countsFor(gone); p != 1 || r != 1 || f != 1 {
		t.Fatalf("fixture: %s counts = %d/%d/%d, want 1/1/1", gone, p, r, f)
	}
	PruneCounts(gone)
	if p, r, f := countsFor(gone); p+r+f != 0 {
		t.Fatalf("after PruneCounts(%s): counts = %d/%d/%d, want none", gone, p, r, f)
	}
	if p, r, f := countsFor(kept); p != 1 || r != 1 || f != 1 {
		t.Fatalf("PruneCounts(%s) touched %s: counts = %d/%d/%d", gone, kept, p, r, f)
	}
	// Counting again after a prune starts from zero (a re-added name).
	countRecordFailure(gone, "ledger_unavailable")
	for _, c := range RecordFailureCounts() {
		if c.Database == gone && c.Count != 1 {
			t.Fatalf("re-added %s starts at %d, want 1", gone, c.Count)
		}
	}
	PruneCounts(gone)
	PruneCounts(kept)
}

// Nil/empty and invalid input: an empty or unknown name prunes nothing.
func TestPruneCountsEmptyOrUnknownNameIsNoop(t *testing.T) {
	db := fmt.Sprintf("prune_noop_%d", time.Now().UnixNano())
	seedPruneCounters(db)
	t.Cleanup(func() { PruneCounts(db) })
	PruneCounts("")
	PruneCounts(db + "_unknown")
	if p, r, f := countsFor(db); p != 1 || r != 1 || f != 1 {
		t.Fatalf("no-op prunes changed %s: counts = %d/%d/%d", db, p, r, f)
	}
}

// Concurrent access: pruning while other goroutines count neither panics
// nor loses the other database's counts.
func TestPruneCountsConcurrentWithCounting(t *testing.T) {
	stamp := time.Now().UnixNano()
	busy, other := fmt.Sprintf("prune_busy_%d", stamp), fmt.Sprintf("prune_other_%d", stamp)
	t.Cleanup(func() { PruneCounts(busy); PruneCounts(other) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); countRecordFailure(other, "x") }()
		go func() { defer wg.Done(); countRecordFailure(busy, "x"); PruneCounts(busy) }()
	}
	wg.Wait()
	for _, c := range RecordFailureCounts() {
		if c.Database == other && c.Count != 8 {
			t.Fatalf("%s count = %d, want 8", other, c.Count)
		}
	}
}
