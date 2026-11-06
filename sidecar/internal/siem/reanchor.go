package siem

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// ReanchorRequest moves one sink's cursor on one chain of one source back
// so that export resumes at FromSeq (1 = the whole chain).
type ReanchorRequest struct {
	Sink    string `json:"sink"`
	Source  string `json:"source"`
	Chain   string `json:"chain"`
	FromSeq int64  `json:"from_seq"`
}

// ReanchorResult is a done re-anchor: export resumes at FromSeq, and the
// links up to ForkTo are re-exported labelled "fork".
type ReanchorResult struct {
	Sink        string `json:"sink"`
	Source      string `json:"source"`
	Chain       string `json:"chain"`
	FromSeq     int64  `json:"from_seq"`
	PreviousSeq int64  `json:"previous_seq"`
	ForkTo      int64  `json:"fork_to"`
}

// Reanchor resets a sink's cursor so export resumes at req.FromSeq, after
// an admin compared the SIEM's copy with the database. It is never
// automatic. It runs on the export leader, under its fence, and waits for
// the sink's running pass, so no drain can move the cursor past it.
func (e *Exporter) Reanchor(ctx context.Context, req ReanchorRequest) (ReanchorResult,
	error) {
	s := e.sink(req.Sink)
	if s == nil {
		return ReanchorResult{}, fmt.Errorf("%w: %q", ErrUnknownSink, req.Sink)
	}
	if _, ok := auditchain.SpecFor(req.Chain); !ok {
		return ReanchorResult{}, fmt.Errorf("%w: unknown chain %q", ErrInvalidReanchor,
			req.Chain)
	}
	if req.FromSeq < 1 {
		return ReanchorResult{}, fmt.Errorf("%w: from_seq must be 1 or more",
			ErrInvalidReanchor)
	}
	fence, ok := e.allow()
	if !ok {
		return ReanchorResult{}, ErrNotLeader
	}
	src, ok := e.source(req.Source)
	if !ok {
		return ReanchorResult{}, fmt.Errorf("%w: %q", ErrUnknownSource, req.Source)
	}
	at, hash, err := anchorBefore(ctx, src, req)
	if err != nil {
		return ReanchorResult{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return ReanchorResult{}, fmt.Errorf("siem: re-anchor: wait for the sink: %w", err)
	}
	defer s.release()
	prev, err := e.cursors.Reanchor(ctx, fence, req.Sink, req.Source, req.Chain, at, hash)
	if err != nil {
		return ReanchorResult{}, err
	}
	s.clearRegression(regressionKey(req.Source, req.Chain))
	e.logf("siem sink %s: cursor of %s/%s re-anchored from %d to %d; links up to %d are "+
		"re-exported labelled %q", req.Sink, req.Source, req.Chain, prev.Seq, at,
		max(prev.ForkTo, prev.Seq), forkLabel)
	return ReanchorResult{Sink: req.Sink, Source: req.Source, Chain: req.Chain,
		FromSeq: req.FromSeq, PreviousSeq: prev.Seq,
		ForkTo: max(prev.ForkTo, prev.Seq)}, nil
}

// anchorBefore reads the link export resumes after: none for FromSeq 1,
// otherwise the link at FromSeq-1, which must exist.
func anchorBefore(ctx context.Context, src Source, req ReanchorRequest) (int64, string,
	error) {
	at := req.FromSeq - 1
	if at == 0 {
		return 0, "", nil
	}
	hash, found, err := auditchain.LinkHashAt(ctx, src.DB, req.Chain, at)
	if err != nil {
		return 0, "", fmt.Errorf("siem: re-anchor: source %s: %w", src.Name, err)
	}
	if !found {
		return 0, "", fmt.Errorf("%w: source %s has no %s link %d to resume after; "+
			"choose a from_seq at most one past the chain's head", ErrInvalidReanchor,
			src.Name, req.Chain, at)
	}
	return at, hash, nil
}

// Cursors returns each sink's cursor on chain of source, for the sinks
// that receive chain. A nil exporter (export off) has none.
func (e *Exporter) Cursors(ctx context.Context, source, chain string) ([]Cursor, error) {
	if e == nil {
		return nil, nil
	}
	var out []Cursor
	for _, s := range e.sinks {
		if !wants(s.cfg.Chains, chain) {
			continue
		}
		c, err := e.cursors.LoadCursor(ctx, s.cfg.Sink.Name(), source, chain)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (e *Exporter) sink(name string) *sinkState {
	for _, s := range e.sinks {
		if s.cfg.Sink.Name() == name {
			return s
		}
	}
	return nil
}

func (e *Exporter) source(name string) (Source, bool) {
	for _, src := range e.sources() {
		if src.Name == name && src.DB != nil {
			return src, true
		}
	}
	return Source{}, false
}
