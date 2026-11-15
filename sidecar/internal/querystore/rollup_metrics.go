package querystore

import (
	"sort"
	"sync"
)

// Process-wide rollup metrics (evid-01a):
// pg_sage_query_rollup_hours_total{database,result} (hours rolled: ok,
// empty, failed) and pg_sage_query_rollup_lag_hours{database} (due hours
// not rolled yet).

type rollupKey struct{ database, result string }

var rollupMetrics = struct {
	mu    sync.Mutex
	hours map[rollupKey]uint64
	lag   map[string]float64
}{hours: map[rollupKey]uint64{}, lag: map[string]float64{}}

// RollupHourCount is one series of pg_sage_query_rollup_hours_total.
type RollupHourCount struct {
	Database string
	Result   string
	Count    uint64
}

// RollupLag is one series of pg_sage_query_rollup_lag_hours.
type RollupLag struct {
	Database string
	Hours    float64
}

func noteRollupHour(database, result string) {
	rollupMetrics.mu.Lock()
	rollupMetrics.hours[rollupKey{database, result}]++
	rollupMetrics.mu.Unlock()
}

func setRollupLag(database string, hours float64) {
	rollupMetrics.mu.Lock()
	rollupMetrics.lag[database] = hours
	rollupMetrics.mu.Unlock()
}

// RollupHourCounts is a sorted snapshot of the hour counters.
func RollupHourCounts() []RollupHourCount {
	rollupMetrics.mu.Lock()
	out := make([]RollupHourCount, 0, len(rollupMetrics.hours))
	for k, n := range rollupMetrics.hours {
		out = append(out, RollupHourCount{Database: k.database, Result: k.result, Count: n})
	}
	rollupMetrics.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Result < out[j].Result
	})
	return out
}

// RollupLags is a sorted snapshot of the lag gauges.
func RollupLags() []RollupLag {
	rollupMetrics.mu.Lock()
	out := make([]RollupLag, 0, len(rollupMetrics.lag))
	for db, h := range rollupMetrics.lag {
		out = append(out, RollupLag{Database: db, Hours: h})
	}
	rollupMetrics.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}
