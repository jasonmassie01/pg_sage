package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// The observe-only sweeper (fleetpol-01d). Observe-only members have no
// runtime: no collector, analyzer, executor, pool, model call or goroutine
// of their own. One fleet-level sweeper visits them in rounds, a bounded
// number at a time, each with one short-lived read-only connection, and
// keeps each member's newest grade in the meta database. A policy set may
// promote a member to full (it then counts against the 50 and gets a
// runtime on the next meta reconcile); only full members can have actions.

// Sweeper bounds.
const (
	DefaultObserveConcurrency   = 4
	MaxObserveConcurrency       = 16
	DefaultObserveInterval      = 15 * time.Minute
	MinObserveInterval          = time.Minute
	DefaultObserveMemberTimeout = time.Minute
	maxObserveErrorLen          = 1000
)

// ErrObserveUnreachable marks a visit that could not connect to the member.
var ErrObserveUnreachable = errors.New("observe-only member unreachable")

// ErrObserveRoundRunning refuses a round while another runs.
var ErrObserveRoundRunning = errors.New("an observe-only sweep round is already running")

// ObserveStore is the meta database the sweeper reads and writes
// (*store.DatabaseStore).
type ObserveStore interface {
	ObserveOnlyMembers(ctx context.Context) ([]store.DatabaseRecord, error)
	GetConnectionString(ctx context.Context, id int) (string, error)
	BeginObserveRound(ctx context.Context, started time.Time, members int) (int64, error)
	RecordObserveSweep(ctx context.Context, sw store.ObserveSweep) error
	FinishObserveRound(ctx context.Context, r store.ObserveRound) error
	SetTier(ctx context.Context, id int, tier string) error
}

// ObserveGrade is what one visit found: the overall letter and the grade.
type ObserveGrade struct {
	Letter string
	Detail json.RawMessage
	// Activity is the visit's workload reading for the sprawl census
	// (fleetpol-03); nil when the check took none.
	Activity *ObserveActivity
}

// ObserveActivity is one visit's census reading: the database's
// pg_stat_statements calls by roles other than pg_sage's (a cumulative
// counter) and its size; either is nil when it could not be read.
type ObserveActivity struct {
	WorkloadCalls *int64
	SizeBytes     *int64
}

// ObserveVisit visits one member; an error wrapping ErrObserveUnreachable
// marks it unreachable, any other error failed.
type ObserveVisit func(ctx context.Context, rec store.DatabaseRecord,
	dsn string) (ObserveGrade, error)

// ObserveOptions configure the sweeper; the zero value is the default.
type ObserveOptions struct {
	Concurrency   int           // members visited at once: 0 is 4, at most 16
	Interval      time.Duration // the round time: 0 is 15 minutes, at least 1
	MemberTimeout time.Duration // one visit's budget: 0 is 1 minute
	Visit         ObserveVisit  // nil: DefaultObserveVisit(DefaultObserveCheck)
	// Promote reports that a policy set promotes the member to full.
	Promote func(store.DatabaseRecord) bool
	// Members receives the observe-only member names after each listing
	// (DatabaseManager.SetObserveOnly), so their actions are refused.
	Members func(names []string)
	// Activity stores a visit's census reading (CensusDB.RecordActivity);
	// nil: readings are not kept.
	Activity func(ctx context.Context, key string, calls, size *int64, at time.Time) error
	After    func(time.Duration) <-chan time.Time // nil: time.After
	Now      func() time.Time
	Logf     func(level, format string, args ...any)
}

// ObserveSweeper visits the observe-only members in rounds.
type ObserveSweeper struct {
	store   ObserveStore
	opts    ObserveOptions
	running sync.Mutex
}

// NewObserveSweeper returns a sweeper over st, or nil without a store.
func NewObserveSweeper(st ObserveStore, opts ObserveOptions) *ObserveSweeper {
	if st == nil {
		return nil
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultObserveConcurrency
	}
	opts.Concurrency = min(opts.Concurrency, MaxObserveConcurrency)
	if opts.Interval <= 0 {
		opts.Interval = DefaultObserveInterval
	}
	opts.Interval = max(opts.Interval, MinObserveInterval)
	if opts.MemberTimeout <= 0 {
		opts.MemberTimeout = DefaultObserveMemberTimeout
	}
	if opts.Visit == nil {
		opts.Visit = DefaultObserveVisit(DefaultObserveCheck(ObserveCheckOptions{}))
	}
	if opts.After == nil {
		opts.After = time.After
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, string, ...any) {}
	}
	return &ObserveSweeper{store: st, opts: opts}
}

// Concurrency is how many members a round visits at once.
func (s *ObserveSweeper) Concurrency() int { return s.opts.Concurrency }

// Interval is the round time: a round starts this long after the last.
func (s *ObserveSweeper) Interval() time.Duration { return s.opts.Interval }

// Run sweeps a round at once, then one every interval, until ctx ends.
func (s *ObserveSweeper) Run(ctx context.Context) {
	for {
		r, err := s.SweepRound(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.opts.Logf("WARN", "observe-only sweep: %v", err)
		case err == nil && r.Members > 0:
			s.opts.Logf("INFO", "observe-only sweep: round %d: %d members in %s "+
				"(%d ok, %d unreachable, %d failed, %d promoted, at most %d at once)",
				r.ID, r.Members, r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond),
				r.OK, r.Unreachable, r.Failed, r.Promoted, r.MaxConcurrent)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.opts.After(s.opts.Interval):
		}
	}
}

// SweepRound lists the observe-only members, promotes those a policy set
// promotes, and visits the rest. A member that cannot be reached or
// graded is recorded so and the round goes on.
func (s *ObserveSweeper) SweepRound(ctx context.Context) (store.ObserveRound, error) {
	if !s.running.TryLock() {
		return store.ObserveRound{}, ErrObserveRoundRunning
	}
	defer s.running.Unlock()
	members, err := s.store.ObserveOnlyMembers(ctx)
	if err != nil {
		return store.ObserveRound{}, fmt.Errorf("observe-only sweep: listing "+
			"observe-only members: %w", err)
	}
	members, promoted := s.promote(ctx, members)
	s.publish(members)
	round := store.ObserveRound{StartedAt: s.opts.Now(), Promoted: promoted}
	if len(members) == 0 {
		round.FinishedAt = round.StartedAt
		return round, nil
	}
	if round.ID, err = s.store.BeginObserveRound(ctx, round.StartedAt,
		len(members)); err != nil {
		return round, fmt.Errorf("observe-only sweep: %w", err)
	}
	round.Members = len(members)
	visitErr := s.visitAll(ctx, members, &round)
	round.FinishedAt = s.opts.Now()
	finishErr := s.store.FinishObserveRound(context.WithoutCancel(ctx), round)
	return round, errors.Join(visitErr, finishErr)
}

// promote moves each member a policy set promotes to full; one at the 50
// cap (or failing to move) stays observe-only and is swept.
func (s *ObserveSweeper) promote(ctx context.Context,
	members []store.DatabaseRecord) ([]store.DatabaseRecord, int) {
	if s.opts.Promote == nil {
		return members, 0
	}
	kept := members[:0:0]
	promoted := 0
	for _, rec := range members {
		if !s.opts.Promote(rec) {
			kept = append(kept, rec)
			continue
		}
		if err := s.store.SetTier(ctx, rec.ID, store.TierFull); err != nil {
			s.opts.Logf("WARN", "observe-only sweep: %s stays observe-only: its policy "+
				"set promotes it to full, but: %v", rec.Name, err)
			kept = append(kept, rec)
			continue
		}
		s.opts.Logf("INFO", "observe-only sweep: %s promoted to a full member by its "+
			"policy set", rec.Name)
		promoted++
	}
	return kept, promoted
}

func (s *ObserveSweeper) publish(members []store.DatabaseRecord) {
	if s.opts.Members == nil {
		return
	}
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.Name
	}
	s.opts.Members(names)
}

// roundTally counts a round's outcomes across its visits.
type roundTally struct {
	mu                  sync.Mutex
	ok, unreach, failed int
	errs                []error
	active, peak        atomic.Int64
}

func (t *roundTally) add(err error) {
	t.mu.Lock()
	t.errs = append(t.errs, err)
	t.mu.Unlock()
}

// visitAll visits members, at most Concurrency at once, until ctx ends.
func (s *ObserveSweeper) visitAll(ctx context.Context, members []store.DatabaseRecord,
	round *store.ObserveRound) error {
	var tally roundTally
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.opts.Concurrency)
	for _, rec := range members {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(rec store.DatabaseRecord) {
			defer wg.Done()
			defer func() { <-sem }()
			s.visitOne(ctx, round.ID, rec, &tally)
		}(rec)
	}
	wg.Wait()
	round.OK, round.Unreachable, round.Failed = tally.ok, tally.unreach, tally.failed
	round.MaxConcurrent = int(tally.peak.Load())
	if err := ctx.Err(); err != nil {
		tally.errs = append(tally.errs, fmt.Errorf("observe-only sweep: round %d stopped: %w",
			round.ID, err))
	}
	return errors.Join(tally.errs...)
}
