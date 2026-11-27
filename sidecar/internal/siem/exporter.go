package siem

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// Source is one database whose audit chains are exported.
type Source struct {
	Name string // database name, or "control"
	DB   auditchain.Querier
}

// SinkConfig is one sink and the chains it receives (empty = all), with
// the configuration warnings its status reports.
type SinkConfig struct {
	Sink     Sink
	Chains   []string
	Warnings []string
}

// Delivery guarantees a sink reports.
const (
	DeliveryAtLeastOnce = "at_least_once" // the receiver confirmed, or the cursor holds
	DeliveryBestEffort  = "best_effort"   // udp: a lost datagram is not retried
)

// delivery is the sink's guarantee: best effort when it says so.
func delivery(s Sink) string {
	if b, ok := s.(interface{ BestEffort() bool }); ok && b.BestEffort() {
		return DeliveryBestEffort
	}
	return DeliveryAtLeastOnce
}

// Options tune delivery.
type Options struct {
	BatchSize  int
	Interval   time.Duration // between passes when caught up
	MaxBackoff time.Duration // longest wait after failures, per lane
	// OnUndeliverable records an event a sink refused and got as a stub
	// (a finding); an error holds the cursor before it. nil only logs.
	OnUndeliverable func(context.Context, Undeliverable) error
}

// SinkStatus is a sink's delivery health. Status is StatusOK,
// StatusFailing (some lane fails) or StatusRegressed (Regressions names the
// stopped chains). Lanes has one entry per source and chain; Undeliverable
// counts the events sent as stubs. Delivery is DeliveryAtLeastOnce or
// DeliveryBestEffort; Warnings lists unsafe settings (token_over_http).
type SinkStatus struct {
	Name                string       `json:"name"`
	Status              string       `json:"status"`
	Delivery            string       `json:"delivery"`
	Warnings            []string     `json:"warnings,omitempty"`
	Delivered           int64        `json:"delivered"`
	Undeliverable       int64        `json:"undeliverable"`
	LastSuccess         time.Time    `json:"last_success"`
	LastError           string       `json:"last_error,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures"`
	Regressions         []Regression `json:"regressions,omitempty"`
	Lanes               []LaneStatus `json:"lanes"`
}

type sinkState struct {
	cfg           SinkConfig
	turn          chan struct{} // held by a pass or a re-anchor
	mu            sync.Mutex
	status        SinkStatus
	regressed     map[string]Regression // by regressionKey
	lanes         map[string]*laneState // by regressionKey; chain "" is the source
	fenceFailures int                   // consecutive failed fence checks
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
		e.sinks = append(e.sinks, &sinkState{cfg: s, turn: make(chan struct{}, 1),
			status: SinkStatus{Name: s.Sink.Name(), Delivery: delivery(s.Sink),
				Warnings: slices.Clone(s.Warnings)},
			regressed: map[string]Regression{}})
	}
	return e
}

// Status reports every sink's delivery health.
func (e *Exporter) Status() []SinkStatus {
	out := make([]SinkStatus, 0, len(e.sinks))
	for _, s := range e.sinks {
		s.mu.Lock()
		out = append(out, s.statusLocked())
		s.mu.Unlock()
	}
	return out
}

// RunOnce makes one delivery pass per sink, in turn, attempting every
// lane now, whatever its backoff.
func (e *Exporter) RunOnce(ctx context.Context) error {
	var errs []error
	for _, s := range e.sinks {
		if err := e.pass(ctx, s, false); err != nil {
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

// loop passes every Interval; a failing lane waits out its own backoff
// inside the passes, and only a failed fence check backs the sink off.
func (e *Exporter) loop(ctx context.Context, s *sinkState) {
	for {
		_ = e.pass(ctx, s, true)
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.backoff(s)):
		}
	}
}

// backoff is the wait before the sink's next pass.
func (e *Exporter) backoff(s *sinkState) time.Duration {
	s.mu.Lock()
	failures := s.fenceFailures
	s.mu.Unlock()
	return e.delay(failures)
}

// pass delivers everything new to one sink. Each chain of each source is a
// lane: a failing lane records its error and, with honourBackoff, waits out
// its own backoff while the pass goes on; a regressed chain stops only
// itself. Only a failed fence check or a lost lease stops the whole pass. A
// pass holds the sink's turn, so a re-anchor never interleaves.
func (e *Exporter) pass(ctx context.Context, s *sinkState, honourBackoff bool) error {
	fence, ok := e.allow()
	if !ok {
		return nil
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if err := e.checkFence(ctx, s, fence); err != nil {
		return err
	}
	visited := map[string]bool{}
	var errs []error
	for _, src := range e.sources() {
		err := e.passSource(ctx, s, src, fence, visited, honourBackoff)
		if err != nil && (errors.Is(err, ErrFenced) || ctx.Err() != nil) {
			return e.fail(s, err)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	s.keepRegressions(visited)
	s.keepLanes(visited)
	if err := errors.Join(errs...); err != nil {
		return e.fail(s, err)
	}
	s.mu.Lock()
	s.status.ConsecutiveFailures, s.status.LastError = 0, s.regressionError()
	s.mu.Unlock()
	return nil
}

// checkFence fails the pass, and backs the whole sink off, while this
// sidecar may not move cursors (a lost lease, an unreachable control
// database).
func (e *Exporter) checkFence(ctx context.Context, s *sinkState, fence Fence) error {
	err := e.cursors.CheckFence(ctx, fence)
	s.mu.Lock()
	if err != nil {
		s.fenceFailures++
	} else {
		s.fenceFailures = 0
	}
	failures := s.fenceFailures
	s.mu.Unlock()
	if err == nil {
		return nil
	}
	if failures == 1 || failures%10 == 0 {
		e.logf("siem sink %s: export check failed (%d in a row), retrying in %s; nothing "+
			"is lost while it fails: %v", s.cfg.Sink.Name(), failures, e.delay(failures), err)
	}
	return e.fail(s, err)
}

// passSource delivers every wanted chain of one source, each chain on its
// own: one failing chain never holds back the next.
func (e *Exporter) passSource(ctx context.Context, s *sinkState, src Source, fence Fence,
	visited map[string]bool, honourBackoff bool) error {
	now := time.Now()
	if err := s.waiting(src.Name, "", now); honourBackoff && err != nil {
		s.keepSource(src.Name, visited)
		return err
	}
	chains, err := auditchain.Installed(ctx, src.DB)
	if err != nil {
		s.keepSource(src.Name, visited)
		if ctx.Err() != nil {
			return err
		}
		return e.laneFailed(s, src.Name, "", err)
	}
	s.laneSucceeded(src.Name, "")
	var errs []error
	for _, chain := range chains {
		if !wants(s.cfg.Chains, chain) {
			continue
		}
		visited[regressionKey(src.Name, chain)] = true
		if err := s.waiting(src.Name, chain, now); honourBackoff && err != nil {
			errs = append(errs, err)
			continue
		}
		err := e.drain(ctx, s, src, chain, fence)
		if err != nil && (errors.Is(err, ErrFenced) || ctx.Err() != nil) {
			return err
		}
		if err != nil {
			errs = append(errs, e.laneFailed(s, src.Name, chain, err))
			continue
		}
		s.laneSucceeded(src.Name, chain)
	}
	return errors.Join(errs...)
}

// drain sends one chain of one source to the sink, batch by batch, once
// the link at the cursor is still the one it exported.
func (e *Exporter) drain(ctx context.Context, s *sinkState, src Source, chain string,
	fence Fence) error {
	cur, err := e.cursors.LoadCursor(ctx, s.cfg.Sink.Name(), src.Name, chain)
	if err != nil {
		return err
	}
	head, _, err := auditchain.Head(ctx, src.DB, chain)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lane(src.Name, chain).track(cur.Seq, head)
	s.mu.Unlock()
	if ok, err := e.anchored(ctx, s, src, cur, fence); err != nil || !ok {
		return err
	}
	r := &laneRun{s: s, src: src, chain: chain, fence: fence, seq: cur.Seq}
	for {
		recs, err := readRecords(ctx, src, chain, r.seq, e.opts.BatchSize)
		if err != nil || len(recs) == 0 {
			return err
		}
		events := make([]Event, 0, len(recs))
		for i := range recs {
			if recs[i].Link.Seq <= cur.ForkTo {
				recs[i].Labels = []string{forkLabel}
			}
			events = append(events, Map(recs[i]))
		}
		if err := e.deliver(ctx, r, recs, events); err != nil {
			return err
		}
		if len(recs) < e.opts.BatchSize {
			return nil
		}
	}
}

// fail counts a failed pass on the sink's status.
func (e *Exporter) fail(s *sinkState, err error) error {
	s.mu.Lock()
	s.status.ConsecutiveFailures++
	s.status.LastError = err.Error()
	s.mu.Unlock()
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
