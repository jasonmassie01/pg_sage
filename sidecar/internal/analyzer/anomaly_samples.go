package analyzer

import (
	"sort"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/collector"
)

// The workload_anomaly metrics (market-02), all from data the collector
// already read: no new query reaches the monitored database.
const (
	// MetricQueryMean is a statement's interval mean in ms (rules-05).
	MetricQueryMean = "query_mean_ms"
	// MetricActiveBackends and MetricTotalBackends are this database's
	// active and total backends at collection.
	MetricActiveBackends = "active_backends"
	MetricTotalBackends  = "total_backends"
	// MetricWALRate is the cluster's WAL bytes per second over the interval.
	MetricWALRate = "wal_bytes_per_s"
	// MetricWaitSessions counts the sessions of the lock graph waiting in
	// one wait class.
	MetricWaitSessions = "wait_sessions"
)

// waitClasses are the pg_stat_activity wait_event_type classes sampled,
// each every cycle (zero included) so a quiet hour is part of the bucket.
var waitClasses = []string{"Activity", "BufferPin", "Client", "Extension", "IO", "IPC",
	"Lock", "LWLock", "Timeout"}

// cycleSamples are one cycle's samples, with each statement's share of
// the interval's DB time and its counters. queriesRead reports a
// statement interval this cycle, and present the identities of all its
// statements (sampled or not).
type cycleSamples struct {
	samples     []baseline.Sample
	share       map[baseline.Key]float64
	queries     map[baseline.Key]collector.QueryStats
	queriesRead bool
	present     map[string]bool
}

// workloadSamples is the cycle's samples: the interval means of at most
// maxKeys measured statements (largest interval time first), the backend
// gauges, the WAL rate and the lock-graph wait classes, each only when
// its category was read.
func workloadSamples(cur, prev *collector.Snapshot, minCalls int64,
	maxKeys int) cycleSamples {
	cs := cycleSamples{share: map[baseline.Key]float64{},
		queries: map[baseline.Key]collector.QueryStats{}, present: map[string]bool{}}
	cs.addQueries(cur, prev, minCalls, maxKeys)
	if cur.Available("system") {
		cs.add(MetricActiveBackends, "database", "gauge", float64(cur.System.ActiveBackends))
		cs.add(MetricTotalBackends, "database", "gauge", float64(cur.System.TotalBackends))
		if iv := cur.System.Interval; iv != nil && iv.WALBytes != nil && iv.WALSeconds > 0 {
			cs.add(MetricWALRate, "cluster", "rate", float64(*iv.WALBytes)/iv.WALSeconds)
		}
	}
	if cur.Available("locks") {
		counts := waitSessions(cur.Locks)
		for _, class := range waitClasses {
			cs.add(MetricWaitSessions, class, "lock_graph_count", float64(counts[class]))
		}
	}
	return cs
}

func (cs *cycleSamples) add(metric, key, how string, v float64) {
	cs.samples = append(cs.samples, baseline.Sample{
		Key: baseline.Key{Metric: metric, Key: key}, How: how, Value: v})
}

// addQueries samples the statements with a measured interval (the
// rules-05 call floor; a reset or an evicted entry has none).
func (cs *cycleSamples) addQueries(cur, prev *collector.Snapshot, minCalls int64,
	maxKeys int) {
	if !hasQueryInterval(cur, prev) {
		return
	}
	cs.queriesRead = true
	for _, q := range adviceQueries(cur) {
		cs.present[queryIdent(q.QueryID)] = true
	}
	if maxKeys <= 0 {
		return
	}
	before := queriesByID(prev)
	var measured []regressionCandidate
	var dbTime float64
	for _, q := range adviceQueries(cur) {
		if iv, ok := measuredInterval(q, before, minCalls); ok {
			measured = append(measured, regressionCandidate{q: q, interval: iv})
			dbTime += iv.TotalMs
		}
	}
	sort.SliceStable(measured, func(i, j int) bool {
		return measured[i].interval.TotalMs > measured[j].interval.TotalMs
	})
	if len(measured) > maxKeys {
		measured = measured[:maxKeys]
	}
	for _, m := range measured {
		k := baseline.Key{Metric: MetricQueryMean, Key: queryIdent(m.q.QueryID)}
		cs.add(k.Metric, k.Key, "interval_mean", m.interval.MeanMs)
		if dbTime > 0 {
			cs.share[k] = m.interval.TotalMs / dbTime
		}
		cs.queries[k] = m.q
	}
}

// waitSessions counts the distinct sessions of the lock graph per wait
// class.
func waitSessions(locks []collector.LockInfo) map[string]int {
	seen := map[string]map[int]bool{}
	for _, l := range locks {
		if l.WaitEventType == nil {
			continue
		}
		class := *l.WaitEventType
		if seen[class] == nil {
			seen[class] = map[int]bool{}
		}
		seen[class][l.PID] = true
	}
	out := make(map[string]int, len(seen))
	for class, pids := range seen {
		out[class] = len(pids)
	}
	return out
}

// sampleKeys are the keys of samples, in order.
func sampleKeys(samples []baseline.Sample) []baseline.Key {
	out := make([]baseline.Key, len(samples))
	for i, s := range samples {
		out[i] = s.Key
	}
	return out
}
