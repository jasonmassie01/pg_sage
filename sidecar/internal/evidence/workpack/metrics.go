package workpack

import (
	"sort"
	"sync"
)

// Process-wide pack metrics: pg_sage_workload_pack_builds_total{database,
// result} and pg_sage_workload_pack_bytes{database} (the last pack built
// or reused).

// Build results.
const (
	ResultBuilt    = "built"
	ResultReused   = "reused"
	ResultTooLarge = "too_large"
	ResultFailed   = "failed"
)

type buildKey struct{ database, result string }

var packMetrics = struct {
	mu     sync.Mutex
	builds map[buildKey]uint64
	bytes  map[string]int64
}{builds: map[buildKey]uint64{}, bytes: map[string]int64{}}

// BuildCount is one series of pg_sage_workload_pack_builds_total.
type BuildCount struct {
	Database string
	Result   string
	Count    uint64
}

// PackSize is one series of pg_sage_workload_pack_bytes.
type PackSize struct {
	Database string
	Bytes    int64
}

// NoteBuild counts a build; a built or reused pack's bytes become the
// database's gauge.
func NoteBuild(database, result string, bytes int64) {
	packMetrics.mu.Lock()
	defer packMetrics.mu.Unlock()
	packMetrics.builds[buildKey{database, result}]++
	if result == ResultBuilt || result == ResultReused {
		packMetrics.bytes[database] = bytes
	}
}

// BuildCounts is a sorted snapshot of the build counters.
func BuildCounts() []BuildCount {
	packMetrics.mu.Lock()
	out := make([]BuildCount, 0, len(packMetrics.builds))
	for k, n := range packMetrics.builds {
		out = append(out, BuildCount{Database: k.database, Result: k.result, Count: n})
	}
	packMetrics.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Result < out[j].Result
	})
	return out
}

// PackBytes is a sorted snapshot of the size gauges.
func PackBytes() []PackSize {
	packMetrics.mu.Lock()
	out := make([]PackSize, 0, len(packMetrics.bytes))
	for db, b := range packMetrics.bytes {
		out = append(out, PackSize{Database: db, Bytes: b})
	}
	packMetrics.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}

// ForgetDatabase drops the series of a database that left the fleet, so
// they leave /metrics with it. An empty name forgets nothing.
func ForgetDatabase(database string) {
	if database == "" {
		return
	}
	packMetrics.mu.Lock()
	defer packMetrics.mu.Unlock()
	delete(packMetrics.bytes, database)
	for k := range packMetrics.builds {
		if k.database == database {
			delete(packMetrics.builds, k)
		}
	}
}
