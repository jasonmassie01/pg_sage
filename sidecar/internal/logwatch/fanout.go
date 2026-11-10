package logwatch

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/pg-sage/sidecar/internal/rca"
)

// maxBufferSize is the maximum number of signals per subscriber
// buffer. When exceeded, low-priority signals are dropped first.
const maxBufferSize = 10000

// LogFanout distributes signals from a single FileWatcher to
// multiple RCA engines. This enables fleet mode where one log directory
// is shared across multiple databases on the same PostgreSQL cluster.
// A signal that names a database goes to every subscriber (each engine
// keeps only its own database's lines); a signal without a database is a
// cluster event and goes only to the cluster owner, the lowest
// subscriber id (rca-08).
//
// Buffers are capped at maxBufferSize with priority-based eviction:
// CRITICAL/FATAL/PANIC signals are never dropped. When the buffer is
// full, the oldest low-priority signal (info, then warning) is
// evicted to make room.
type LogFanout struct {
	source     *FileWatcher
	clusterKey string
	buffers    map[string][]*rca.Signal     // subscriberID -> pending
	live       map[string]*FanoutSubscriber // subscriberID -> current handle
	mu         sync.Mutex
}

// NewLogFanout creates a fanout adapter around the given FileWatcher.
func NewLogFanout(source *FileWatcher) *LogFanout {
	return &LogFanout{
		source:  source,
		buffers: make(map[string][]*rca.Signal),
		live:    make(map[string]*FanoutSubscriber),
	}
}

// WithClusterKey names the cluster (host:port) in the scope tags of the
// cluster events the owner receives.
func (f *LogFanout) WithClusterKey(key string) *LogFanout {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clusterKey = key
	return f
}

// Subscribe returns a LogSource that receives the signals of the
// underlying FileWatcher meant for it. The id should be unique per
// consumer (typically the database name). Subscribing an id again keeps
// its undrained backlog for the new handle; the old handle's Drain and
// Stop then do nothing.
func (f *LogFanout) Subscribe(id string) *FanoutSubscriber {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buffers[id]; !ok {
		f.buffers[id] = nil
	}
	sub := &FanoutSubscriber{fanout: f, id: id}
	f.live[id] = sub
	return sub
}

// SubscribeEntries registers a parsed-entry consumer on the shared watcher.
// The database filter prevents one fleet database from analyzing another
// database's statements while preserving one cluster-level file offset.
func (f *LogFanout) SubscribeEntries(
	id string, database string,
) *EntrySubscriber {
	return f.source.SubscribeEntries(id, database)
}

// DrainSource drains the underlying FileWatcher once and hands each
// signal to its subscribers. The caller must invoke this periodically
// (e.g. on a ticker goroutine). Buffers are capped at maxBufferSize with
// priority-based eviction.
func (f *LogFanout) DrainSource() {
	f.distribute(f.source.Drain())
}

// distribute appends each database line to every subscriber's buffer and
// each cluster event, tagged with its scope, to the owner's only, so a
// fleet raises one incident per cluster event, not one per database.
// With no subscriber the signals are dropped.
func (f *LogFanout) distribute(signals []*rca.Signal) {
	if len(signals) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := slices.Sorted(maps.Keys(f.buffers))
	if len(ids) == 0 {
		return
	}
	owned, shared := f.route(signals, ids)
	for _, id := range ids {
		batch := shared
		if id == ids[0] {
			batch = owned
		}
		f.buffers[id] = appendCapped(f.buffers[id], slices.Clone(batch))
	}
}

// route splits signals, in arrival order, into the owner's batch (every
// signal, cluster events tagged) and the other subscribers' batch
// (database lines only).
func (f *LogFanout) route(
	signals []*rca.Signal, ids []string,
) (owned, shared []*rca.Signal) {
	for _, s := range signals {
		if s == nil {
			continue
		}
		if db, _ := s.Metrics["database"].(string); db != "" {
			owned = append(owned, s)
			shared = append(shared, s)
			continue
		}
		owned = append(owned, clusterScoped(s, f.clusterKey, ids))
	}
	return owned, shared
}

// clusterScoped returns a copy of s tagged as a cluster event: scope
// "cluster", the cluster key when known, and the databases (subscriber
// ids) of the cluster.
func clusterScoped(s *rca.Signal, key string, ids []string) *rca.Signal {
	cp := *s
	cp.Metrics = make(map[string]any, len(s.Metrics)+3)
	maps.Copy(cp.Metrics, s.Metrics)
	cp.Metrics["scope"] = "cluster"
	if key != "" {
		cp.Metrics["cluster_key"] = key
	}
	cp.Metrics["databases"] = slices.Clone(ids)
	return &cp
}

// isClusterScoped reports whether s is a cluster event the fanout tagged.
func isClusterScoped(s *rca.Signal) bool {
	scope, _ := s.Metrics["scope"].(string)
	return scope == "cluster"
}

// DroppedLines reports the lines the shared watcher lost since it started.
func (f *LogFanout) DroppedLines() DropCounts {
	return f.source.DroppedLines()
}

// Stop shuts down the underlying FileWatcher.
func (f *LogFanout) Stop() {
	f.source.Stop()
}

// drain returns and clears the buffered signals of the subscriber.
// A stopped or replaced handle gets nothing and recreates no buffer.
func (f *LogFanout) drain(sub *FanoutSubscriber) []*rca.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live[sub.id] != sub {
		return nil
	}
	buf := f.buffers[sub.id]
	f.buffers[sub.id] = nil
	return buf
}

// drainCritical returns and removes the subscriber's critical signals, in
// order, leaving the others buffered for the analyzer cycle.
func (f *LogFanout) drainCritical(sub *FanoutSubscriber) []*rca.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live[sub.id] != sub {
		return nil
	}
	buf := f.buffers[sub.id]
	var critical []*rca.Signal
	rest := buf[:0]
	for _, s := range buf {
		if signalPriority(s) >= 3 {
			critical = append(critical, s)
			continue
		}
		rest = append(rest, s)
	}
	clear(buf[len(rest):])
	f.buffers[sub.id] = rest
	return critical
}

// unsubscribe deletes the subscriber's buffer. When it owned the cluster,
// ownership passes to the next subscriber id together with the cluster
// events it had not drained yet, so removing a database loses no PANIC.
func (f *LogFanout) unsubscribe(sub *FanoutSubscriber) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live[sub.id] != sub {
		return
	}
	ids := slices.Sorted(maps.Keys(f.buffers))
	pending := f.buffers[sub.id]
	delete(f.live, sub.id)
	delete(f.buffers, sub.id)
	if len(ids) < 2 || ids[0] != sub.id {
		return
	}
	var cluster []*rca.Signal
	for _, s := range pending {
		if isClusterScoped(s) {
			cluster = append(cluster, s)
		}
	}
	f.buffers[ids[1]] = appendCapped(f.buffers[ids[1]], cluster)
}

// signalPriority returns a numeric priority for a signal's severity.
// Higher values are more important and should never be dropped.
func signalPriority(s *rca.Signal) int {
	switch s.Severity {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

// appendCapped appends new signals to buf, enforcing maxBufferSize.
// When the buffer would exceed capacity, the oldest signal with the
// lowest priority is evicted. Signals with severity "critical" are
// never evicted.
func appendCapped(buf, incoming []*rca.Signal) []*rca.Signal {
	buf = append(buf, incoming...)
	for len(buf) > maxBufferSize {
		victim := findLowestPriorityIndex(buf)
		if victim < 0 {
			// All signals are critical — drop oldest.
			buf = buf[1:]
		} else {
			buf = append(buf[:victim], buf[victim+1:]...)
		}
	}
	return buf
}

// findLowestPriorityIndex returns the index of the oldest signal
// with the lowest priority that is NOT critical. Returns -1 if all
// signals are critical.
func findLowestPriorityIndex(buf []*rca.Signal) int {
	lowestPri := 4 // higher than any real priority
	lowestIdx := -1
	for i, s := range buf {
		p := signalPriority(s)
		if p >= 3 {
			continue // never evict critical
		}
		if p < lowestPri {
			lowestPri = p
			lowestIdx = i
		}
	}
	return lowestIdx
}

// FanoutSubscriber implements rca.LogSource so it can be passed to
// rcaEng.SetLogSource(). Start is a no-op because the underlying
// FileWatcher lifecycle is owned by the LogFanout; Stop unsubscribes.
type FanoutSubscriber struct {
	fanout *LogFanout
	id     string
}

// compile-time interface checks
var (
	_ rca.LogSource         = (*FanoutSubscriber)(nil)
	_ rca.CriticalLogSource = (*FanoutSubscriber)(nil)
)

// Start is a no-op — the source FileWatcher is already started by
// the LogFanout owner.
func (s *FanoutSubscriber) Start(_ context.Context) error {
	return nil
}

// Drain returns all signals buffered since the last Drain call for
// this subscriber.
func (s *FanoutSubscriber) Drain() []*rca.Signal {
	return s.fanout.drain(s)
}

// DrainCritical returns the critical signals buffered since the last
// drain and leaves the rest for Drain: the RCA log fast path takes these
// between analyzer cycles (rca-06).
func (s *FanoutSubscriber) DrainCritical() []*rca.Signal {
	return s.fanout.drainCritical(s)
}

// Stop unsubscribes: the buffer is deleted (a database removed at runtime
// keeps no backlog) and cluster ownership moves on (rca-08). The source
// FileWatcher is stopped by the LogFanout owner. Stop is idempotent.
func (s *FanoutSubscriber) Stop() {
	s.fanout.unsubscribe(s)
}
