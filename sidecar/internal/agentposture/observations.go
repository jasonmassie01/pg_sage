package agentposture

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// ObservedValue is one measured thing in an observation: a size, a
// cumulative delete count and the tables it covers.
type ObservedValue struct {
	Bytes   int64
	Deletes int64
	Tables  []string
}

// Observation is what a detector measured at one time, by key.
type Observation struct {
	At     time.Time
	Values map[string]ObservedValue
}

func (o Observation) clone() Observation {
	return Observation{At: o.At, Values: cloneValues(o.Values)}
}

func cloneValues(in map[string]ObservedValue) map[string]ObservedValue {
	out := maps.Clone(in)
	for k, v := range out {
		v.Tables = slices.Clone(v.Tables)
		out[k] = v
	}
	return out
}

// ObservationStore keeps each detector's latest observation between
// posture runs, for detectors that compare two timed observations
// (AP-12). It lives in memory with the Monitor of one database: a restart
// forgets it, so the next comparison waits for a second observation.
// The first look has none (nil); every method is nil-safe.
type ObservationStore struct {
	mu  sync.Mutex
	obs map[string]Observation
}

// NewObservationStore returns an empty store.
func NewObservationStore() *ObservationStore {
	return &ObservationStore{obs: map[string]Observation{}}
}

// Previous returns a copy of detector id's stored observation.
func (s *ObservationStore) Previous(id string) (Observation, bool) {
	if s == nil {
		return Observation{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.obs[id]
	if !ok {
		return Observation{}, false
	}
	return o.clone(), true
}

// Record stores a copy of o as detector id's observation.
func (s *ObservationStore) Record(id string, o Observation) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.obs[id] = o.clone()
}
