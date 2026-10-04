package shadow

import (
	"sort"
	"sync"
	"testing"
)

// The Prometheus counters of shadow mode (cmd renders them): decisions by
// database, class and the gate's verdict had the class been trusted, and
// scores by database, class, score and source. Counters only grow and are
// safe for concurrent cycles.

func TestCountersAccumulateByLabels(t *testing.T) {
	before := decisionCount("m1", "vacuum", "execute")
	countDecision("m1", "vacuum", "execute")
	countDecision("m1", "vacuum", "execute")
	countDecision("m1", "vacuum", "queue_approval")
	if got := decisionCount("m1", "vacuum", "execute") - before; got != 2 {
		t.Fatalf("execute decisions = %d, want 2", got)
	}
	if decisionCount("m1", "vacuum", "queue_approval") == 0 {
		t.Fatal("queue_approval label not counted separately")
	}
	sb := scoreCount("m1", "index_create", ScoreCorrect, SourceHypoPG)
	countScore("m1", "index_create", ScoreCorrect, SourceHypoPG)
	if scoreCount("m1", "index_create", ScoreCorrect, SourceHypoPG)-sb != 1 {
		t.Fatal("score not counted")
	}
	if scoreCount("m1", "index_create", ScoreIncorrect, SourceHypoPG) != 0 {
		t.Fatal("a score leaked into another label set")
	}
}

func TestCountersAreSortedAndConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	before := decisionCount("m2", "analyze", "execute")
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			countDecision("m2", "analyze", "execute")
			_ = DecisionCounts()
		}()
	}
	wg.Wait()
	if got := decisionCount("m2", "analyze", "execute") - before; got != 50 {
		t.Fatalf("concurrent count = %d, want 50", got)
	}
	all := DecisionCounts()
	if !sort.SliceIsSorted(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Verdict < b.Verdict
	}) {
		t.Fatalf("decision counters are not sorted: %+v", all)
	}
}

// A decision without a class or verdict is not counted (nothing could be
// attributed); a database without a name (standalone) is.
func TestCountersIgnoreUnattributableDecisions(t *testing.T) {
	n := len(DecisionCounts())
	countDecision("m3", "", "execute")
	countDecision("m3", "vacuum", "")
	countScore("m3", "", ScoreCorrect, SourceHypoPG)
	if len(DecisionCounts()) != n || scoreCount("m3", "", ScoreCorrect, SourceHypoPG) != 0 {
		t.Fatal("a counter without a class or verdict was created")
	}
	before := decisionCount("", "vacuum", "execute")
	countDecision("", "vacuum", "execute")
	if decisionCount("", "vacuum", "execute")-before != 1 {
		t.Fatal("a standalone database's decision was not counted")
	}
}
