package siem

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// Source is one database whose audit chains are exported.
type Source struct {
	Name string // database name, or "control"
	DB   auditchain.Querier
}

// SinkConfig is one sink and the chains it receives (empty = all).
type SinkConfig struct {
	Sink   Sink
	Chains []string
}

// Options tune delivery.
type Options struct {
	BatchSize  int
	Interval   time.Duration // between passes when caught up
	MaxBackoff time.Duration // longest wait after failures
}

// SinkStatus is a sink's delivery health.
type SinkStatus struct {
	Name                string    `json:"name"`
	Delivered           int64     `json:"delivered"`
	LastSuccess         time.Time `json:"last_success"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

type sinkState struct {
	cfg    SinkConfig
	mu     sync.Mutex
	status SinkStatus
}

// Exporter ships every audit chain link to every sink.
type Exporter struct {
	sources func() []Source
	sinks   []*sinkState
	cursors *CursorStore
	opts    Options
	allow   func() (Fence, bool)
	logf    func(string, ...any)
}

// NewExporter builds an exporter. allow reports whether this sidecar may
// export now (the leader) and the lease to fence cursor writes with.
func NewExporter(sources func() []Source, sinks []SinkConfig, cursors *CursorStore,
	opts Options, allow func() (Fence, bool), logf func(string, ...any)) *Exporter {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 200
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.MaxBackoff < opts.Interval {
		opts.MaxBackoff = opts.Interval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	e := &Exporter{sources: sources, cursors: cursors, opts: opts, allow: allow, logf: logf}
	for _, s := range sinks {
		e.sinks = append(e.sinks, &sinkState{cfg: s,
			status: SinkStatus{Name: s.Sink.Name()}})
	}
	return e
}

// Status reports every sink's delivery health.
func (e *Exporter) Status() []SinkStatus {
	out := make([]SinkStatus, 0, len(e.sinks))
	for _, s := range e.sinks {
		s.mu.Lock()
		out = append(out, s.status)
		s.mu.Unlock()
	}
	return out
}

// RunOnce makes one delivery pass per sink, in turn.
func (e *Exporter) RunOnce(ctx context.Context) error {
	var errs []error
	for _, s := range e.sinks {
		if err := e.pass(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run delivers until ctx ends, one goroutine per sink so a slow or failing
// sink never holds another back. It never touches the writers' path.
func (e *Exporter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range e.sinks {
		wg.Add(1)
		go func(s *sinkState) {
			defer wg.Done()
			e.loop(ctx, s)
		}(s)
	}
	wg.Wait()
}

func (e *Exporter) loop(ctx context.Context, s *sinkState) {
	for {
		wait := e.opts.Interval
		if err := e.pass(ctx, s); err != nil && ctx.Err() == nil {
			wait = e.backoff(s)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// backoff doubles the wait per consecutive failure up to MaxBackoff.
func (e *Exporter) backoff(s *sinkState) time.Duration {
	s.mu.Lock()
	failures := s.status.ConsecutiveFailures
	s.mu.Unlock()
	wait := e.opts.Interval
	for i := 1; i < failures && wait < e.opts.MaxBackoff; i++ {
		wait *= 2
	}
	return min(wait, e.opts.MaxBackoff)
}

// pass delivers everything new to one sink. A failure stops the pass with
// the cursor on the last delivered batch.
func (e *Exporter) pass(ctx context.Context, s *sinkState) error {
	fence, ok := e.allow()
	if !ok {
		return nil
	}
	if err := e.cursors.CheckFence(ctx, fence); err != nil {
		return e.fail(s, err)
	}
	for _, src := range e.sources() {
		chains, err := auditchain.Installed(ctx, src.DB)
		if err != nil {
			return e.fail(s, fmt.Errorf("source %s: %w", src.Name, err))
		}
		for _, chain := range chains {
			if !wants(s.cfg.Chains, chain) {
				continue
			}
			if err := e.drain(ctx, s, src, chain, fence); err != nil {
				return e.fail(s, err)
			}
		}
	}
	s.mu.Lock()
	s.status.ConsecutiveFailures, s.status.LastError = 0, ""
	s.mu.Unlock()
	return nil
}

// drain sends one chain of one source to the sink, batch by batch.
func (e *Exporter) drain(ctx context.Context, s *sinkState, src Source, chain string,
	fence Fence) error {
	name := s.cfg.Sink.Name()
	for {
		after, err := e.cursors.Load(ctx, name, src.Name, chain)
		if err != nil {
			return err
		}
		recs, err := readRecords(ctx, src, chain, after, e.opts.BatchSize)
		if err != nil || len(recs) == 0 {
			return err
		}
		events := make([]Event, 0, len(recs))
		for _, r := range recs {
			events = append(events, Map(r))
		}
		if err := s.cfg.Sink.Send(ctx, events); err != nil {
			return err
		}
		last := recs[len(recs)-1].Link.Seq
		if err := e.cursors.Save(ctx, fence, name, src.Name, chain, last); err != nil {
			return err
		}
		s.mu.Lock()
		s.status.Delivered += int64(len(recs))
		s.status.LastSuccess = time.Now().UTC()
		s.mu.Unlock()
		if len(recs) < e.opts.BatchSize {
			return nil
		}
	}
}

func (e *Exporter) fail(s *sinkState, err error) error {
	s.mu.Lock()
	s.status.ConsecutiveFailures++
	s.status.LastError = err.Error()
	failures := s.status.ConsecutiveFailures
	s.mu.Unlock()
	if failures == 1 || failures%10 == 0 {
		e.logf("siem sink %s: delivery failed (%d in a row), retrying; nothing is "+
			"lost while it fails: %v", s.cfg.Sink.Name(), failures, err)
	}
	return fmt.Errorf("siem sink %s: %w", s.cfg.Sink.Name(), err)
}

func wants(filter []string, chain string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, c := range filter {
		if c == chain {
			return true
		}
	}
	return false
}
