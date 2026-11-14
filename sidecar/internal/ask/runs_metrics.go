package ask

import (
	"sort"
	"sync"
)

// Process-wide run metrics: pg_sage_ask_runs_active{database} (runs in
// progress on this sidecar) and pg_sage_ask_runs_total{database,state}
// (runs that ended, by terminal state).

type runKey struct{ database, state string }

var runCounters = struct {
	mu     sync.Mutex
	active map[string]int64
	ended  map[runKey]uint64
}{active: map[string]int64{}, ended: map[runKey]uint64{}}

// RunCount is one series of pg_sage_ask_runs_total.
type RunCount struct {
	Database string
	State    string
	Count    uint64
}

// ActiveRun is one series of pg_sage_ask_runs_active.
type ActiveRun struct {
	Database string
	Active   int64
}

func runStarted(database string) {
	runCounters.mu.Lock()
	runCounters.active[database]++
	runCounters.mu.Unlock()
}

func runEnded(database, state string) {
	runCounters.mu.Lock()
	defer runCounters.mu.Unlock()
	if runCounters.active[database] > 0 {
		runCounters.active[database]--
	}
	runCounters.ended[runKey{database, state}]++
}

// RunCounts is a sorted snapshot of the ended-run counters.
func RunCounts() []RunCount {
	runCounters.mu.Lock()
	out := make([]RunCount, 0, len(runCounters.ended))
	for k, n := range runCounters.ended {
		out = append(out, RunCount{Database: k.database, State: k.state, Count: n})
	}
	runCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].State < out[j].State
	})
	return out
}

// ActiveRuns is a sorted snapshot of the runs in progress per database.
func ActiveRuns() []ActiveRun {
	runCounters.mu.Lock()
	out := make([]ActiveRun, 0, len(runCounters.active))
	for db, n := range runCounters.active {
		out = append(out, ActiveRun{Database: db, Active: n})
	}
	runCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}

// forgetRuns drops a database's run series.
func forgetRuns(database string) {
	runCounters.mu.Lock()
	defer runCounters.mu.Unlock()
	delete(runCounters.active, database)
	for k := range runCounters.ended {
		if k.database == database {
			delete(runCounters.ended, k)
		}
	}
}
