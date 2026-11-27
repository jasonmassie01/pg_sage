package siem

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Lane states.
const (
	LaneOK        = "ok"
	LaneFailing   = "failing"   // deliveries fail; retried with this lane's backoff
	LanePoisoned  = "poisoned"  // the receiver refuses an event and its stub; the lane holds
	LaneRegressed = "regressed" // the chain went back behind the cursor; waits for a re-anchor
)

// LaneStatus is one sink's delivery of one chain of one source. A lane
// with an empty Chain is the source itself, when its chains could not be
// listed (an unreachable database). Lag is Head minus Cursor at the last
// attempt.
type LaneStatus struct {
	Source              string `json:"source"`
	Chain               string `json:"chain"`
	Cursor              int64  `json:"cursor"`
	Head                int64  `json:"head"`
	Lag                 int64  `json:"lag"`
	LastError           string `json:"last_error,omitempty"`
	State               string `json:"state"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// laneState is what a sink knows of one lane between passes.
type laneState struct {
	source, chain string
	cursor, head  int64
	failures      int
	lastError     string
	poisoned      bool
	next          time.Time // no attempt before this while failing
}

func (l *laneState) track(cursor, head int64) { l.cursor, l.head = cursor, max(head, cursor) }

// lane returns the state of one lane, creating it; s.mu must be held.
func (s *sinkState) lane(source, chain string) *laneState {
	if s.lanes == nil {
		s.lanes = map[string]*laneState{}
	}
	key := regressionKey(source, chain)
	l, ok := s.lanes[key]
	if !ok {
		l = &laneState{source: source, chain: chain}
		s.lanes[key] = l
	}
	return l
}

// waiting returns the error of a failing lane still inside its backoff,
// nil when the lane may be attempted now.
func (s *sinkState) waiting(source, chain string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lanes[regressionKey(source, chain)]
	if !ok || l.failures == 0 || !now.Before(l.next) {
		return nil
	}
	return fmt.Errorf("%s: retrying at %s: %s", laneName(source, chain),
		l.next.UTC().Format(time.RFC3339), l.lastError)
}

func laneName(source, chain string) string {
	if chain == "" {
		return "source " + source
	}
	return "source " + source + ", chain " + chain
}

// laneFailed records a failed attempt and schedules the next one.
func (e *Exporter) laneFailed(s *sinkState, source, chain string, err error) error {
	s.mu.Lock()
	l := s.lane(source, chain)
	l.failures++
	l.lastError = err.Error()
	l.poisoned = errors.Is(err, errPoisoned)
	wait := e.delay(l.failures)
	l.next = time.Now().Add(wait)
	failures := l.failures
	s.mu.Unlock()
	if failures == 1 || failures%10 == 0 {
		e.logf("siem sink %s: %s: delivery failed (%d in a row), retrying in %s; other "+
			"sources and chains keep exporting, and nothing is lost while it fails: %v",
			s.cfg.Sink.Name(), laneName(source, chain), failures, wait, err)
	}
	return fmt.Errorf("%s: %w", laneName(source, chain), err)
}

// laneSucceeded clears a lane's failures; a source lane (no chain) that
// lists its chains again is dropped.
func (s *sinkState) laneSucceeded(source, chain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if chain == "" {
		delete(s.lanes, regressionKey(source, chain))
		return
	}
	l := s.lane(source, chain)
	l.failures, l.lastError, l.poisoned, l.next = 0, "", false, time.Time{}
}

// keepSource marks every lane and regression of source visited, for a
// source this pass could not read: its state stays until it can.
func (s *sinkState) keepSource(source string, visited map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := regressionKey(source, "")
	for key := range s.lanes {
		if strings.HasPrefix(key, prefix) {
			visited[key] = true
		}
	}
	for key := range s.regressed {
		if strings.HasPrefix(key, prefix) {
			visited[key] = true
		}
	}
	visited[prefix] = true
}

// keepLanes drops the lanes a pass no longer visited (a removed source or
// chain).
func (s *sinkState) keepLanes(visited map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.lanes {
		if !visited[key] {
			delete(s.lanes, key)
		}
	}
}

// lanesLocked lists the lanes in source/chain order; s.mu must be held.
func (s *sinkState) lanesLocked() []LaneStatus {
	out := make([]LaneStatus, 0, len(s.lanes))
	for key, l := range s.lanes {
		st := LaneStatus{Source: l.source, Chain: l.chain, Cursor: l.cursor, Head: l.head,
			Lag: max(l.head-l.cursor, 0), LastError: l.lastError, State: LaneOK,
			ConsecutiveFailures: l.failures}
		switch _, regressed := s.regressed[key]; {
		case regressed:
			st.State = LaneRegressed
		case l.poisoned:
			st.State = LanePoisoned
		case l.failures > 0:
			st.State = LaneFailing
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Source < b.Source || (a.Source == b.Source && a.Chain < b.Chain)
	})
	return out
}

// delay is the wait after failures consecutive failures: Interval,
// doubled per further failure, at most MaxBackoff.
func (e *Exporter) delay(failures int) time.Duration {
	wait := e.opts.Interval
	for i := 1; i < failures && wait < e.opts.MaxBackoff; i++ {
		wait *= 2
	}
	return min(wait, e.opts.MaxBackoff)
}
