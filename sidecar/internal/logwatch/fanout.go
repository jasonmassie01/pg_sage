package logwatch

import (
	"context"
	"sync"

	"github.com/pg-sage/sidecar/internal/rca"
)

// maxBufferSize is the maximum number of signals per subscriber
// buffer. When exceeded, low-priority signals are dropped first.
const maxBufferSize = 10000

// LogFanout distributes signals from a single FileWatcher to
// multiple RCA engines. Each subscriber gets a copy of every signal.
// This enables fleet mode where one log directory is shared across
// multiple databases on the same PostgreSQL cluster.
//
// Buffers are capped at maxBufferSize with priority-based eviction:
// CRITICAL/FATAL/PANIC signals are never dropped. When the buffer is
// full, the oldest low-priority signal (info, then warning) is
// evicted to make room.
type LogFanout struct {
	source  *FileWatcher
	buffers map[string][]*rca.Signal // subscriberID -> pending
	mu      sync.Mutex
}

// NewLogFanout creates a fanout adapter around the given FileWatcher.
func NewLogFanout(source *FileWatcher) *LogFanout {
	return &LogFanout{
		source:  source,
		buffers: make(map[string][]*rca.Signal),
	}
}

// Subscribe returns a LogSource that receives copies of all signals
// from the underlying FileWatcher. The id should be unique per
// consumer (typically the database name).
func (f *LogFanout) Subscribe(id string) *FanoutSubscriber {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buffers[id] = nil
	return &FanoutSubscriber{fanout: f, id: id}
}

// DrainSource drains the underlying FileWatcher once and appends
// a copy of each signal to every subscriber's buffer. The caller
// must invoke this periodically (e.g. on a ticker goroutine).
// Buffers are capped at maxBufferSize with priority-based eviction.
func (f *LogFanout) DrainSource() {
	signals := f.source.Drain()
	if len(signals) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.buffers {
		cp := make([]*rca.Signal, len(signals))
		copy(cp, signals)
		f.buffers[id] = appendCapped(f.buffers[id], cp)
	}
}

// Stop shuts down the underlying FileWatcher.
func (f *LogFanout) Stop() {
	f.source.Stop()
}

// drain returns and clears buffered signals for the given subscriber.
func (f *LogFanout) drain(id string) []*rca.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	buf := f.buffers[id]
	f.buffers[id] = nil
	return buf
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
// rcaEng.SetLogSource(). Start and Stop are no-ops because the
// underlying FileWatcher lifecycle is owned by the LogFanout.
type FanoutSubscriber struct {
	fanout *LogFanout
	id     string
}

// compile-time interface check
var _ rca.LogSource = (*FanoutSubscriber)(nil)

// Start is a no-op — the source FileWatcher is already started by
// the LogFanout owner.
func (s *FanoutSubscriber) Start(_ context.Context) error {
	return nil
}

// Drain returns all signals buffered since the last Drain call for
// this subscriber.
func (s *FanoutSubscriber) Drain() []*rca.Signal {
	return s.fanout.drain(s.id)
}

// Stop is a no-op — the source FileWatcher is stopped by the
// LogFanout owner.
func (s *FanoutSubscriber) Stop() {}
