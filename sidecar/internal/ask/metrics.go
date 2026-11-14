package ask

import (
	"sort"
	"sync"
)

// Process-wide answer counters, exported as
// pg_sage_ask_answers_total{database,status,stop}: how many questions
// concluded and how many the wall clock or a cancel cut short.

type answerKey struct{ database, status, stop string }

var answerCounters = struct {
	mu sync.Mutex
	n  map[answerKey]uint64
}{n: map[answerKey]uint64{}}

// AnswerCount is one series of pg_sage_ask_answers_total.
type AnswerCount struct {
	Database string
	Status   string
	Stop     string
	Count    uint64
}

func countAnswer(database string, a Answer) {
	answerCounters.mu.Lock()
	answerCounters.n[answerKey{database, a.Status, a.Stop}]++
	answerCounters.mu.Unlock()
}

// AnswerCounts is a sorted snapshot of the answer counters.
func AnswerCounts() []AnswerCount {
	answerCounters.mu.Lock()
	out := make([]AnswerCount, 0, len(answerCounters.n))
	for k, n := range answerCounters.n {
		out = append(out, AnswerCount{Database: k.database, Status: k.status, Stop: k.stop,
			Count: n})
	}
	answerCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		if a.Status != b.Status {
			return a.Status < b.Status
		}
		return a.Stop < b.Stop
	})
	return out
}

// ForgetDatabase drops the answer and run series of a database that left
// the fleet, so they leave /metrics with it. An empty name forgets
// nothing.
func ForgetDatabase(database string) {
	if database == "" {
		return
	}
	forgetRuns(database)
	answerCounters.mu.Lock()
	defer answerCounters.mu.Unlock()
	for k := range answerCounters.n {
		if k.database == database {
			delete(answerCounters.n, k)
		}
	}
}
