package shadow

import (
	"sync"
	"testing"
)

// observability-07: a database removed from the process loses its shadow
// counters, so /metrics stops exporting its series.

// seriesOf counts the decision and score series of database.
func seriesOf(database string) (decisions, scores int) {
	for _, c := range DecisionCounts() {
		if c.Database == database {
			decisions++
		}
	}
	for _, c := range ScoreCounts() {
		if c.Database == database {
			scores++
		}
	}
	return decisions, scores
}

// Happy path and state transition: Prune drops both counter kinds of one
// database and keeps the others; counting again starts from zero.
func TestPruneDropsOnlyThatDatabase(t *testing.T) {
	gone, kept := "prune_gone_db", "prune_kept_db"
	t.Cleanup(func() { Prune(gone); Prune(kept) })
	for _, db := range []string{gone, kept} {
		countDecision(db, "vacuum", "execute")
		countDecision(db, "vacuum", "queue_approval")
		countScore(db, "index_create", ScoreCorrect, SourceHypoPG)
	}
	if d, s := seriesOf(gone); d != 2 || s != 1 {
		t.Fatalf("fixture: %s series = %d/%d, want 2/1", gone, d, s)
	}
	Prune(gone)
	if d, s := seriesOf(gone); d+s != 0 {
		t.Fatalf("after Prune(%s): series = %d/%d, want none", gone, d, s)
	}
	if d, s := seriesOf(kept); d != 2 || s != 1 {
		t.Fatalf("Prune(%s) touched %s: series = %d/%d", gone, kept, d, s)
	}
	countDecision(gone, "vacuum", "execute")
	if got := decisionCount(gone, "vacuum", "execute"); got != 1 {
		t.Fatalf("re-added %s starts at %d, want 1", gone, got)
	}
}

// Nil/empty and invalid input: an unknown name prunes nothing. The empty
// name is a real key here (a decision without a database is counted), so
// Prune("") drops only those.
func TestPruneUnknownNameIsNoop(t *testing.T) {
	db := "prune_noop_db"
	t.Cleanup(func() { Prune(db) })
	countDecision(db, "analyze", "execute")
	Prune(db + "_unknown")
	if got := decisionCount(db, "analyze", "execute"); got != 1 {
		t.Fatalf("unknown-name prune changed %s: %d, want 1", db, got)
	}
}

// Concurrent access: Prune races counting without losing other databases.
func TestPruneConcurrentWithCounting(t *testing.T) {
	busy, other := "prune_busy_db", "prune_other_db"
	t.Cleanup(func() { Prune(busy); Prune(other) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); countScore(other, "vacuum", ScoreCorrect, SourceHypoPG) }()
		go func() { defer wg.Done(); countDecision(busy, "vacuum", "execute"); Prune(busy) }()
	}
	wg.Wait()
	if got := scoreCount(other, "vacuum", ScoreCorrect, SourceHypoPG); got != 8 {
		t.Fatalf("%s scores = %d, want 8", other, got)
	}
}
