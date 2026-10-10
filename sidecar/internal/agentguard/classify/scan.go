package classify

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Scan page bounds.
const (
	DefaultScanLimit = 500
	MaxScanLimit     = 2000
)

// Seen remembers the columns already offered to the model, so a column the
// model passed over is not offered again on every pass (the model is paid
// per call). It is per process; a restart offers each column once more.
type Seen struct {
	mu   sync.Mutex
	keys map[Cursor]bool
}

// NewSeen returns an empty set.
func NewSeen() *Seen { return &Seen{keys: map[Cursor]bool{}} }

// fresh returns the columns not offered yet and marks them offered.
func (s *Seen) fresh(cols []Column) []Column {
	if s == nil {
		return cols
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Column
	for _, c := range cols {
		k := Cursor{RelID: c.RelID, AttNum: c.AttNum}
		if !s.keys[k] {
			s.keys[k] = true
			out = append(out, c)
		}
	}
	return out
}

// ScanOptions is one scan page.
type ScanOptions struct {
	After Cursor
	Limit int
	// Model proposes for the columns the rules left alone; nil uses the
	// rules only (the deterministic fallback).
	Model Suggester
	// Seen skips columns the model was already shown; nil offers all.
	Seen *Seen
}

// ScanResult is what one page did.
type ScanResult struct {
	Read     int    `json:"read"`
	Proposed int    `json:"proposed"`
	Next     Cursor `json:"next"`
	Done     bool   `json:"done"`
	// ModelErr is a failed model call; the rule proposals were still kept.
	ModelErr error `json:"-"`
}

// Scan reads one page of unclassified user columns and records the rules'
// proposals, then the model's. It never confirms anything. A failed model
// call is reported in ModelErr and does not fail the scan.
func Scan(ctx context.Context, s *Store, opts ScanOptions) (ScanResult, error) {
	if s == nil || s.pool == nil {
		return ScanResult{}, ErrNoStore
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultScanLimit
	}
	limit = min(limit, MaxScanLimit)
	cols, next, err := s.UnclassifiedColumns(ctx, opts.After, limit)
	if err != nil {
		return ScanResult{}, err
	}
	res := ScanResult{Read: len(cols), Next: next, Done: len(cols) < limit}
	if res.Done {
		res.Next = Cursor{}
	}
	props := Suggest(cols)
	if opts.Model != nil {
		mp, err := modelProposals(ctx, opts.Model, untouched(cols, props), opts.Seen)
		res.ModelErr = err
		props = append(props, mp...)
	}
	for _, p := range props {
		_, created, err := s.Propose(ctx, p)
		if errors.Is(err, ErrColumnNotFound) {
			continue // dropped since it was read
		}
		if err != nil {
			return res, err
		}
		if created {
			res.Proposed++
		}
	}
	return res, nil
}

// untouched lists the columns no rule proposed for.
func untouched(cols []Column, props []Proposal) []Column {
	taken := make(map[Cursor]bool, len(props))
	for _, p := range props {
		taken[Cursor{RelID: p.Column.RelID, AttNum: p.Column.AttNum}] = true
	}
	var out []Column
	for _, c := range cols {
		if !taken[Cursor{RelID: c.RelID, AttNum: c.AttNum}] {
			out = append(out, c)
		}
	}
	return out
}

// modelProposals asks the model about cols in bounded batches; the first
// failure stops it and is returned with what earlier batches proposed.
func modelProposals(ctx context.Context, m Suggester, cols []Column, seen *Seen) (
	[]Proposal, error) {
	var out []Proposal
	for start := 0; start < len(cols); start += maxModelColumns {
		batch := seen.fresh(cols[start:min(start+maxModelColumns, len(cols))])
		if len(batch) == 0 {
			continue
		}
		got, err := m.Suggest(ctx, batch)
		if err != nil {
			return out, fmt.Errorf("model classification proposals: %w", err)
		}
		out = append(out, got...)
	}
	return out, nil
}
