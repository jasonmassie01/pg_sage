package logwatch

import (
	"context"
	"sync"

	"github.com/pg-sage/sidecar/internal/rca"
)

// LogFanout distributes signals from a single FileWatcher to
// multiple RCA engines. Each subscriber gets a copy of every signal.
// This enables fleet mode where one log directory is shared across
// multiple databases on the same PostgreSQL cluster.
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
		f.buffers[id] = append(f.buffers[id], cp...)
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
