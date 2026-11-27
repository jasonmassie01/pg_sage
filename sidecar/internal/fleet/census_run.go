package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/cost"
)

// Census job bounds.
const (
	DefaultCensusConcurrency   = 4
	DefaultCensusEvery         = time.Hour // how often the leader checks the month's run
	DefaultCensusMemberTimeout = time.Minute
)

// CensusStore is the meta database a census run reads and writes
// (*CensusStore).
type CensusStore interface {
	// BeginCensus claims period's run for holder; ok is false when another
	// sidecar holds or finished it.
	BeginCensus(ctx context.Context, period, runAt time.Time, holder string) (int64, bool,
		error)
	CensusSubjects(ctx context.Context) ([]CensusSubject, error)
	// WriteCensus stores run's rows and summary in one transaction.
	WriteCensus(ctx context.Context, run CensusRun, rows []CensusRow) error
}

// CensusMeasureFunc reads one subject's activity and size. An error
// wrapping ErrCensusUnreachable lists it unreachable, any other unmeasured.
type CensusMeasureFunc func(ctx context.Context, s CensusSubject) (CensusMeasure, error)

// CensusOptions configure a Census; the zero value is the default.
type CensusOptions struct {
	Holder        string            // this sidecar's name in the run row
	Rates         cost.RateSource   // operator rates; nil: every row unpriced
	Measure       CensusMeasureFunc // nil: every member unmeasured
	Concurrency   int               // members measured at once: 0 is 4
	MemberTimeout time.Duration     // one measurement's budget: 0 is 1 minute
	Every         time.Duration     // the leader's check: 0 is 1 hour
	IsLeader      func() bool       // nil: always (leader election disabled)
	After         func(time.Duration) <-chan time.Time
	Now           func() time.Time
	Logf          func(level, format string, args ...any)
}

// Census runs the monthly sprawl census.
type Census struct {
	store CensusStore
	opts  CensusOptions
}

// NewCensus returns a census over st, or nil without a store.
func NewCensus(st CensusStore, o CensusOptions) *Census {
	if st == nil {
		return nil
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultCensusConcurrency
	}
	if o.MemberTimeout <= 0 {
		o.MemberTimeout = DefaultCensusMemberTimeout
	}
	if o.Every <= 0 {
		o.Every = DefaultCensusEvery
	}
	if o.Measure == nil {
		o.Measure = func(context.Context, CensusSubject) (CensusMeasure, error) {
			return CensusMeasure{Unmeasured: "no measurer is configured"}, nil
		}
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, string, ...any) {}
	}
	return &Census{store: st, opts: o}
}

// Run checks every Every whether this month's census is due and, on the
// leader, runs it, until ctx ends.
func (c *Census) Run(ctx context.Context) {
	for {
		run, ok, err := c.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			c.opts.Logf("WARN", "sprawl census: %v", err)
		case ok:
			c.opts.Logf("INFO", "sprawl census: run %d for %s: %d databases, %d "+
				"agent-created, reuse %s, %d TTL proposals, %d unreachable", run.ID,
				run.Period.Format("2006-01"), run.Databases, run.AgentCreated,
				shareText(run), run.Proposals, run.Unreachable)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.opts.After(c.opts.Every):
		}
	}
}

func shareText(r CensusRun) string {
	if r.ReuseShare == nil {
		return "unknown (no agent-created database is old enough to judge)"
	}
	return fmt.Sprintf("%d of %d (%.0f%%)", r.Reused, r.ReuseEligible, *r.ReuseShare*100)
}

// RunOnce runs this month's census unless this sidecar is not the leader
// or the month's run is another's or done (ok false, nil error).
func (c *Census) RunOnce(ctx context.Context) (CensusRun, bool, error) {
	if c.opts.IsLeader != nil && !c.opts.IsLeader() {
		return CensusRun{}, false, nil
	}
	now := c.opts.Now()
	run := CensusRun{Period: CensusPeriod(now), Holder: c.opts.Holder, RunAt: now}
	id, ok, err := c.store.BeginCensus(ctx, run.Period, now, c.opts.Holder)
	if err != nil {
		return run, false, fmt.Errorf("claiming the %s census: %w",
			run.Period.Format("2006-01"), err)
	}
	if !ok {
		return run, false, nil
	}
	run.ID = id
	subjects, err := c.store.CensusSubjects(ctx)
	if err != nil {
		return run, false, fmt.Errorf("census run %d: listing the census subjects: %w",
			id, err)
	}
	rows := c.measureAll(ctx, subjects, now)
	if err := ctx.Err(); err != nil {
		return run, false, fmt.Errorf("census run %d stopped: %w", id, err)
	}
	run = SummarizeCensus(run, rows)
	if err := c.store.WriteCensus(ctx, run, rows); err != nil {
		return run, false, fmt.Errorf("census run %d: %w", id, err)
	}
	return run, true, nil
}

// measureAll measures the subjects, at most Concurrency at once; rows keep
// the subjects' order and are never nil.
func (c *Census) measureAll(ctx context.Context, subjects []CensusSubject,
	now time.Time) []CensusRow {
	rows := make([]CensusRow, len(subjects))
	sem := make(chan struct{}, c.opts.Concurrency)
	var wg sync.WaitGroup
	for i, s := range subjects {
		if s.Kind == CensusDiscovered {
			rows[i] = CensusRowFor(s, CensusMeasure{}, nil, c.opts.Rates, now)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			mctx, cancel := context.WithTimeout(ctx, c.opts.MemberTimeout)
			defer cancel()
			m, err := c.opts.Measure(mctx, s)
			rows[i] = CensusRowFor(s, m, err, c.opts.Rates, now)
		}()
	}
	wg.Wait()
	return rows
}
