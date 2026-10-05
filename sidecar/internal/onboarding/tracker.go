package onboarding

import (
	"sort"
	"sync"
	"time"
)

// Metrics is one database's first-run measurement for this process.
type Metrics struct {
	Database          string
	FirstLookDone     bool
	FirstLookItems    int
	FirstLookDuration time.Duration
	HasTTFF           bool
	TTFF              time.Duration
}

type run struct {
	started time.Time
	m       Metrics
}

// Tracker measures, per database runtime, the time from start to the first
// finding (pg_sage_time_to_first_finding_seconds). It is safe for
// concurrent use; a nil Tracker records nothing.
type Tracker struct {
	mu   sync.Mutex
	runs map[string]*run
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{runs: map[string]*run{}} }

// Start begins a measurement for database (a new runtime generation
// starts over).
func (t *Tracker) Start(database string, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runs[database] = &run{started: at, m: Metrics{Database: database}}
}

// FirstLook records a finished first look; one with items is the first
// finding. It returns the time to first finding when this call set it.
func (t *Tracker) FirstLook(database string, at time.Time, items int,
	took time.Duration) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[database]
	if r == nil {
		return 0, false
	}
	r.m.FirstLookDone, r.m.FirstLookItems, r.m.FirstLookDuration = true, items, took
	if items == 0 {
		return 0, false
	}
	return r.first(at)
}

// FirstFinding records the first finding of any source; it returns the
// time to first finding when this call set it.
func (t *Tracker) FirstFinding(database string, at time.Time) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[database]
	if r == nil {
		return 0, false
	}
	return r.first(at)
}

func (r *run) first(at time.Time) (time.Duration, bool) {
	if r.m.HasTTFF {
		return 0, false
	}
	r.m.HasTTFF, r.m.TTFF = true, max(at.Sub(r.started), 0)
	return r.m.TTFF, true
}

// Forget drops database (removed from the fleet).
func (t *Tracker) Forget(database string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.runs, database)
}

// Snapshot returns every database's metrics, sorted by name.
func (t *Tracker) Snapshot() []Metrics {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Metrics, 0, len(t.runs))
	for _, r := range t.runs {
		out = append(out, r.m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}

// ForgetRun drops database only if its measurement is still the one that
// started at started, so a removed runtime never erases its replacement's.
func (t *Tracker) ForgetRun(database string, started time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.runs[database]; r != nil && r.started.Equal(started) {
		delete(t.runs, database)
	}
}
