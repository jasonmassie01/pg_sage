package gitops

import (
	"errors"
	"sync"
	"time"
)

// Report is the last reconcile as the Settings banner shows it: its diff,
// or its error with every problem.
type Report struct {
	Directory string    `json:"directory"`
	Mode      string    `json:"mode"`
	Hash      string    `json:"hash"`
	At        time.Time `json:"at"`
	Changes   []Change  `json:"changes"`
	Applied   bool      `json:"applied"`
	Follower  bool      `json:"follower,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
	Error     string    `json:"error,omitempty"`
	Problems  []Problem `json:"problems,omitempty"`
}

// Status holds the last Report; it is safe for concurrent use.
type Status struct {
	mu   sync.Mutex
	last Report
}

// Record keeps one reconcile's outcome.
func (s *Status) Record(dir string, res Result, err error, at time.Time) {
	r := Report{Directory: dir, Mode: res.Mode, Hash: res.Hash, At: at,
		Changes: res.Changes, Applied: res.Applied, Follower: res.Follower}
	if err != nil {
		r.Error, r.Changes, r.Applied = err.Error(), nil, false
		var invalid *InvalidError
		if errors.As(err, &invalid) {
			r.Problems = invalid.Problems
		}
		r.Missing = errors.Is(err, ErrDirectoryMissing)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = r
}

// Report is the last recorded reconcile (zero before the first).
func (s *Status) Report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.last
	out.Changes = append([]Change(nil), s.last.Changes...)
	out.Problems = append([]Problem(nil), s.last.Problems...)
	return out
}
