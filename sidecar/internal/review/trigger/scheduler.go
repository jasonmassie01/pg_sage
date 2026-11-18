package trigger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Scheduler is one database's trigger loop. Run Tick only on the leader;
// the queue is safe for two at once anyway (SKIP LOCKED claims, one
// running review run per database).
type Scheduler struct {
	Store    *Store
	Database string
	// Owner names this sidecar on its claims.
	Owner string
	// Interval is review.min_interval_hours; Lease bounds a claim.
	Interval time.Duration
	Lease    time.Duration
	Reviewer Reviewer
	Sources  []Source
	// SourceEvery spaces the source polls (0: every tick).
	SourceEvery time.Duration
	Now         func() time.Time
	Log         func(level, format string, args ...any)

	lastSources time.Time
}

// TickResult is what one tick did.
type TickResult struct {
	Fired    int
	RunID    int64
	Triggers []Trigger
	// Resumed: a dead leader's finished run was consumed, nothing ran.
	Resumed bool
	// Busy: another run was running, the claim was released.
	Busy bool
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) log(level, format string, args ...any) {
	if s.Log != nil {
		s.Log(level, format, args...)
	}
}

// Tick polls the sources, returns expired claims to the queue, consumes a
// dead leader's finished run, and starts the run that is due, if any.
func (s *Scheduler) Tick(ctx context.Context) (TickResult, error) {
	now := s.now()
	res := TickResult{Fired: s.poll(ctx, now)}
	if _, err := s.Store.ReleaseExpired(ctx, s.Database, now); err != nil {
		return res, err
	}
	pending, err := s.Store.Pending(ctx, s.Database)
	if err != nil {
		return res, err
	}
	fresh, err := s.resume(ctx, pending, &res)
	if err != nil || res.Resumed {
		return res, err
	}
	sortByFired(fresh)
	group, due := Due(fresh, s.Interval, now)
	if !due {
		return res, nil
	}
	return s.run(ctx, group, now, res)
}

// poll fires what each source returns; a failing source is logged and
// the others still run.
func (s *Scheduler) poll(ctx context.Context, now time.Time) int {
	if s.SourceEvery > 0 && !s.lastSources.IsZero() &&
		now.Before(s.lastSources.Add(s.SourceEvery)) {
		return 0
	}
	s.lastSources = now
	fired := 0
	for _, src := range s.Sources {
		ts, err := src.Evaluate(ctx, now)
		if err != nil {
			s.log("WARN", "review trigger %s for %s: %v", src.Kind(), s.Database, err)
			continue
		}
		for _, t := range ts {
			_, ok, err := s.Store.Fire(ctx, t)
			if err != nil {
				s.log("WARN", "%v", err)
				continue
			}
			if ok {
				fired++
				s.log("INFO", "review trigger %s fired for %s (%s)", t.Kind, s.Database, t.Key)
			}
		}
	}
	return fired
}

// resume settles triggers that a run already started (their leader died):
// a finished run consumes them, a running one keeps them waiting, a
// failed or missing one, or one running for longer than the lease (its
// sidecar died mid-run; the next run's reaper fails it), lets them start a
// new run. It returns the rest.
func (s *Scheduler) resume(ctx context.Context, pending []Trigger, res *TickResult) (
	[]Trigger, error) {
	byRun := map[int64][]int64{}
	var fresh []Trigger
	for _, t := range pending {
		if t.RunID == 0 {
			fresh = append(fresh, t)
			continue
		}
		byRun[t.RunID] = append(byRun[t.RunID], t.ID)
	}
	for run, ids := range byRun {
		st, err := s.Store.RunStatus(ctx, run, s.Lease)
		if err != nil {
			return nil, err
		}
		switch st {
		case "done", "partial":
			if err := s.Store.Consume(ctx, ids, run); err != nil &&
				!errors.Is(err, ErrLeaseLost) {
				return nil, err
			}
			res.Resumed, res.RunID = true, run
			s.log("INFO", "review run %d of %s consumed its triggers after a restart",
				run, s.Database)
		case "running":
		default:
			if err := s.Store.Detach(ctx, ids, run); err != nil &&
				!errors.Is(err, ErrLeaseLost) {
				return nil, err
			}
			fresh = append(fresh, detached(pending, ids)...)
		}
	}
	return fresh, nil
}

func detached(pending []Trigger, ids []int64) []Trigger {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []Trigger
	for _, t := range pending {
		if want[t.ID] {
			t.RunID = 0
			out = append(out, t)
		}
	}
	sortByFired(out)
	return out
}

// run claims group, starts its review run and consumes the claim.
func (s *Scheduler) run(ctx context.Context, group []Trigger, now time.Time,
	res TickResult) (TickResult, error) {
	sortByFired(group)
	ids := make([]int64, 0, len(group))
	for _, t := range group {
		ids = append(ids, t.ID)
	}
	claimed, err := s.Store.Claim(ctx, s.Database, ids, s.Owner, now.Add(s.Lease))
	if err != nil || len(claimed) == 0 {
		return res, err
	}
	sortByFired(claimed)
	ids = ids[:0]
	for _, t := range claimed {
		ids = append(ids, t.ID)
	}
	run := Run{Database: s.Database, Triggers: claimed,
		OnStart: func(ctx context.Context, id int64) error {
			return s.Store.Attach(ctx, ids, id, s.Owner)
		}}
	runID, err := s.Reviewer.Review(ctx, run)
	if err != nil {
		if rerr := s.Store.Release(ctx, ids, s.Owner); rerr != nil {
			s.log("WARN", "review triggers of %s stay claimed until their lease ends: %v",
				s.Database, rerr)
		}
		if errors.Is(err, ErrBusy) {
			res.Busy = true
			return res, nil
		}
		return res, fmt.Errorf("review run of %s for %v: %w", s.Database, run.Kinds(), err)
	}
	if err := s.Store.Consume(ctx, ids, runID); err != nil {
		return res, err
	}
	countRun(claimed)
	res.RunID, res.Triggers = runID, claimed
	return res, nil
}

// sortByFired orders ts oldest first, then by id.
func sortByFired(ts []Trigger) {
	sort.SliceStable(ts, func(i, j int) bool {
		if !ts[i].FiredAt.Equal(ts[j].FiredAt) {
			return ts[i].FiredAt.Before(ts[j].FiredAt)
		}
		return ts[i].ID < ts[j].ID
	})
}
