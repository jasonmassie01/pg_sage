package pgaudit

import (
	"sort"
	"sync"
)

// Outcomes of pgaudit records, the outcome label of
// pg_sage_pgaudit_records_total.
const (
	OutcomeStored    = "stored"    // kept in sage.guard_pgaudit_events
	OutcomeDropped   = "dropped"   // lost: a batch that kept failing, or past a cap
	OutcomeEvicted   = "evicted"   // pushed out of a full log subscription unread
	OutcomeRequeued  = "requeued"  // in a failed batch, kept for the next attempt
	OutcomeUnmatched = "unmatched" // no principal or action of pg_sage's to keep it for
)

// RecordCount is how many pgaudit records of one database had one outcome.
type RecordCount struct {
	Database string
	Outcome  string
	Count    int64
}

type recordKey struct{ database, outcome string }

var recordCounters = struct {
	mu     sync.Mutex
	counts map[recordKey]int64
}{counts: map[recordKey]int64{}}

// CountRecords adds n records of database with outcome; n of 0 or below
// counts nothing.
func CountRecords(database, outcome string, n int) {
	if n <= 0 {
		return
	}
	recordCounters.mu.Lock()
	defer recordCounters.mu.Unlock()
	recordCounters.counts[recordKey{database, outcome}] += int64(n)
}

// RecordCounts is a sorted snapshot of the record counters.
func RecordCounts() []RecordCount {
	recordCounters.mu.Lock()
	out := make([]RecordCount, 0, len(recordCounters.counts))
	for k, n := range recordCounters.counts {
		out = append(out, RecordCount{Database: k.database, Outcome: k.outcome, Count: n})
	}
	recordCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Outcome < out[j].Outcome
	})
	return out
}
