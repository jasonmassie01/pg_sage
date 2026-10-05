package cloudtel

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Status is a database's telemetry as served to operators.
type Status struct {
	Database      string               `json:"database"`
	Provider      string               `json:"provider"`
	Resource      string               `json:"resource,omitempty"`
	Available     bool                 `json:"available"`
	Reason        string               `json:"reason,omitempty"`
	LastPollAt    *time.Time           `json:"last_poll_at,omitempty"`
	LastSuccessAt *time.Time           `json:"last_success_at,omitempty"`
	LastError     string               `json:"last_error,omitempty"`
	Sample        *Sample              `json:"sample,omitempty"`
	Runway        Runway               `json:"storage_runway"`
	Withhold      []string             `json:"withhold"`
	Limits        Limits               `json:"limits"`
	Drift         []managedparam.Drift `json:"parameter_drift"`
}

// Status reports the runtime's state; Reason starts with "unavailable: "
// whenever no fresh sample backs the guards.
func (r *Runtime) Status() Status {
	now := r.now()
	r.mu.RLock()
	st := Status{Database: r.database, Provider: r.provider, Limits: r.limits,
		LastError: r.lastErr, Withhold: []string{},
		Drift: append([]managedparam.Drift{}, r.drift...)}
	if !r.lastPoll.IsZero() {
		t := r.lastPoll
		st.LastPollAt = &t
	}
	if !r.lastSuccess.IsZero() {
		t := r.lastSuccess
		st.LastSuccessAt = &t
	}
	var sample *Sample
	if r.sample != nil {
		c := r.sample.clone()
		sample = &c
	}
	history := r.history
	r.mu.RUnlock()
	switch {
	case r.reason != "":
		st.Reason = "unavailable: " + r.reason
	case sample == nil && st.LastError != "":
		st.Reason = "unavailable: " + st.LastError
	case sample == nil:
		st.Reason = "unavailable: waiting for the first sample"
	default:
		st.Available, st.Sample, st.Resource = true, sample, sample.Resource
		st.Runway = StorageRunway(history, now)
		st.Withhold = append(st.Withhold, Withhold(*sample, st.Runway, now, r.limits)...)
	}
	if math.IsInf(st.Runway.Hours, 1) {
		st.Runway.Hours = -1 // JSON has no infinity: -1 means "not running out"
	}
	return st
}

var registry = struct {
	sync.RWMutex
	m map[string]*Runtime
}{m: map[string]*Runtime{}}

// Register publishes a database's runtime to the API.
func Register(r *Runtime) {
	if r == nil {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	registry.m[r.database] = r
}

// Unregister removes r if it is still the registered runtime.
func Unregister(r *Runtime) {
	if r == nil {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	if registry.m[r.database] == r {
		delete(registry.m, r.database)
	}
}

// Lookup returns the database's runtime (nil: none).
func Lookup(database string) *Runtime {
	registry.RLock()
	defer registry.RUnlock()
	return registry.m[database]
}

// Statuses reports every registered runtime, sorted by database.
func Statuses() []Status {
	registry.RLock()
	rts := make([]*Runtime, 0, len(registry.m))
	for _, r := range registry.m {
		rts = append(rts, r)
	}
	registry.RUnlock()
	out := make([]Status, 0, len(rts))
	for _, r := range rts {
		out = append(out, r.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}
