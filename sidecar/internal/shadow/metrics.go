package shadow

import (
	"sort"
	"sync"
)

// Process-wide counters for /metrics: shadow decisions recorded, by
// database, class and the gate's verdict had the class been trusted, and
// shadow scores, by database, class, score and source.

// DecisionCount is one decision counter.
type DecisionCount struct {
	Database, Class, Verdict string
	Count                    uint64
}

// ScoreCount is one score counter.
type ScoreCount struct {
	Database, Class, Score, Source string
	Count                          uint64
}

var counters = struct {
	mu        sync.Mutex
	decisions map[DecisionCount]uint64
	scores    map[ScoreCount]uint64
}{decisions: map[DecisionCount]uint64{}, scores: map[ScoreCount]uint64{}}

// countDecision counts one recorded decision; one without a class or a
// verdict cannot be attributed and is not counted.
func countDecision(database, class, verdict string) {
	if class == "" || verdict == "" {
		return
	}
	counters.mu.Lock()
	defer counters.mu.Unlock()
	counters.decisions[DecisionCount{Database: database, Class: class, Verdict: verdict}]++
}

// countScore counts one score written.
func countScore(database, class, score, source string) {
	if class == "" || score == "" || source == "" {
		return
	}
	counters.mu.Lock()
	defer counters.mu.Unlock()
	counters.scores[ScoreCount{Database: database, Class: class, Score: score,
		Source: source}]++
}

// DecisionCounts is a sorted snapshot of the decision counters.
func DecisionCounts() []DecisionCount {
	counters.mu.Lock()
	out := make([]DecisionCount, 0, len(counters.decisions))
	for k, n := range counters.decisions {
		k.Count = n
		out = append(out, k)
	}
	counters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Verdict < b.Verdict
	})
	return out
}

// ScoreCounts is a sorted snapshot of the score counters.
func ScoreCounts() []ScoreCount {
	counters.mu.Lock()
	out := make([]ScoreCount, 0, len(counters.scores))
	for k, n := range counters.scores {
		k.Count = n
		out = append(out, k)
	}
	counters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		for _, p := range [][2]string{{a.Database, b.Database}, {a.Class, b.Class},
			{a.Score, b.Score}} {
			if p[0] != p[1] {
				return p[0] < p[1]
			}
		}
		return a.Source < b.Source
	})
	return out
}
