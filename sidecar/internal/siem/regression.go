package siem

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// Sink statuses.
const (
	StatusOK        = "ok"
	StatusFailing   = "failing"   // deliveries fail; retried with backoff
	StatusRegressed = "regressed" // a chain went back behind the cursor; it waits for a re-anchor
)

// Re-anchor refusals.
var (
	ErrUnknownSink   = errors.New("siem: no such sink")
	ErrUnknownSource = errors.New("siem: no such source database")
	ErrNotLeader     = errors.New("siem: this sidecar is not the export leader; " +
		"retry on the leader")
)

// forkLabel marks events re-exported after a re-anchor: their positions
// were exported before, with other links.
const forkLabel = "fork"

// Regression is one chain whose export stopped for a sink because the
// link at the sink's cursor is gone or is another link.
type Regression struct {
	Source string    `json:"source"`
	Chain  string    `json:"chain"`
	Seq    int64     `json:"seq"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

func (r Regression) message() string {
	return fmt.Sprintf("source %s, chain %s: export stopped at position %d: %s", r.Source,
		r.Chain, r.Seq, r.Reason)
}

func regressionKey(source, chain string) string { return source + "\x00" + chain }

// Anchor is what a source holds at a cursor's position.
type Anchor struct {
	OK     bool   // the link the cursor last exported is still there
	Hash   string // that position's hash now ("" when it is missing)
	Reason string // why not, when !OK
}

// CheckAnchor reports whether the link c last exported is still at its
// position on db. A cursor without a hash (saved before hashes were kept)
// checks existence only; one at 0 has exported nothing to check.
func CheckAnchor(ctx context.Context, db auditchain.Querier, c Cursor) (Anchor, error) {
	if c.Seq <= 0 {
		return Anchor{OK: true}, nil
	}
	hash, found, err := auditchain.LinkHashAt(ctx, db, c.Chain, c.Seq)
	if err != nil {
		return Anchor{}, err
	}
	if !found {
		return Anchor{Reason: fmt.Sprintf("link %d, the last one exported, is missing: "+
			"the chain went back (restore, PITR or deleted links)", c.Seq)}, nil
	}
	if c.Hash != "" && hash != c.Hash {
		return Anchor{Hash: hash, Reason: fmt.Sprintf("link %d is not the link exported "+
			"there (hash %.12s, exported %.12s): the chain went back and its positions "+
			"were reused", c.Seq, hash, c.Hash)}, nil
	}
	return Anchor{OK: true, Hash: hash}, nil
}

// anchored checks the cursor before a drain: a regression stops this
// chain for this sink (reported, not an error), a cursor without a hash
// gets its link's hash.
func (e *Exporter) anchored(ctx context.Context, s *sinkState, src Source, cur Cursor,
	fence Fence) (bool, error) {
	a, err := CheckAnchor(ctx, src.DB, cur)
	if err != nil {
		return false, fmt.Errorf("check the export cursor: %w", err)
	}
	if !a.OK {
		e.regress(s, Regression{Source: src.Name, Chain: cur.Chain, Seq: cur.Seq,
			Reason: a.Reason, Since: time.Now().UTC()})
		return false, nil
	}
	s.clearRegression(regressionKey(src.Name, cur.Chain))
	if cur.Seq > 0 && cur.Hash == "" {
		if err := e.cursors.BackfillHash(ctx, fence, cur.Sink, cur.Source, cur.Chain,
			cur.Seq, a.Hash); err != nil {
			return false, err
		}
	}
	return true, nil
}

// regress records a stopped chain, logging it the first time.
func (e *Exporter) regress(s *sinkState, r Regression) {
	key := regressionKey(r.Source, r.Chain)
	s.mu.Lock()
	_, known := s.regressed[key]
	if !known {
		s.regressed[key] = r
	}
	s.mu.Unlock()
	if !known {
		e.logf("siem sink %s: %s. Nothing after it is exported until an admin "+
			"re-anchors the cursor (POST /api/v1/audit/siem/%s/reanchor) after comparing "+
			"the SIEM's copy with the database", s.cfg.Sink.Name(), r.message(),
			s.cfg.Sink.Name())
	}
}

func (s *sinkState) clearRegression(key string) {
	s.mu.Lock()
	delete(s.regressed, key)
	s.mu.Unlock()
}

// keepRegressions drops the regressions of chains a full pass no longer
// visited (a removed source or chain).
func (s *sinkState) keepRegressions(visited map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.regressed {
		if !visited[key] {
			delete(s.regressed, key)
		}
	}
}

// statusLocked is the sink's status; s.mu must be held.
func (s *sinkState) statusLocked() SinkStatus {
	st := s.status
	st.Warnings = slices.Clone(st.Warnings)
	st.Status = StatusOK
	st.Lanes = s.lanesLocked()
	if st.ConsecutiveFailures > 0 {
		st.Status = StatusFailing
	}
	if len(s.regressed) == 0 {
		return st
	}
	st.Status = StatusRegressed
	st.Regressions = make([]Regression, 0, len(s.regressed))
	for _, r := range s.regressed {
		st.Regressions = append(st.Regressions, r)
	}
	sort.Slice(st.Regressions, func(i, j int) bool {
		a, b := st.Regressions[i], st.Regressions[j]
		return a.Source < b.Source || (a.Source == b.Source && a.Chain < b.Chain)
	})
	return st
}

// regressionError is the sink's last error while chains are stopped.
func (s *sinkState) regressionError() string {
	st := s.statusLocked()
	if len(st.Regressions) == 0 {
		return ""
	}
	msg := st.Regressions[0].message()
	if n := len(st.Regressions); n > 1 {
		msg += fmt.Sprintf(" (and %d more)", n-1)
	}
	return msg
}

func (s *sinkState) acquire(ctx context.Context) error {
	select {
	case s.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *sinkState) release() { <-s.turn }
