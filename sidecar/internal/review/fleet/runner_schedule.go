package fleet

import (
	"context"
	"fmt"
)

// schedule is tier 4: while the fleet is not stopped, the month's room
// (DeepMax less the deep reviews it already scheduled) goes to the top
// outliers, with tokens split by need; the rest wait for next month. A
// member the scheduler refuses, or one left by an emergency stop, waits
// for this month.
func (r *Runner) schedule(ctx context.Context, res *Result, outliers []Outlier) error {
	if len(outliers) == 0 {
		return nil
	}
	if r.stopped() {
		res.Halted = true
		return r.deferAll(ctx, res, outliers, res.Period, false)
	}
	done, err := r.Ledger.ScheduledIn(ctx, res.Period)
	if err != nil {
		return fmt.Errorf("count the deep reviews of %s: %w", res.Period, err)
	}
	pick, rest := SelectDeep(notIn(outliers, done), r.Settings.DeepMax-len(done))
	tokens := AllocateTokens(pick)
	for i, o := range pick {
		if r.stopped() {
			res.Halted = true
			if err := r.deferAll(ctx, res, pick[i:], res.Period, false); err != nil {
				return err
			}
			break
		}
		if err := r.scheduleOne(ctx, res, o, tokens[o.Database]); err != nil {
			return err
		}
	}
	return r.deferAll(ctx, res, rest, NextPeriod(res.Period), true)
}

func (r *Runner) scheduleOne(ctx context.Context, res *Result, o Outlier, tokens int) error {
	d := Deep{Database: o.Database, PassID: o.PassID, Tokens: tokens, Score: o.Score,
		Reason: o.Reason, Period: res.Period}
	ref, err := r.Deep.ScheduleDeep(ctx, d)
	if err != nil {
		if res.Failures == nil {
			res.Failures = map[string]string{}
		}
		res.Failures[o.Database] = err.Error()
		return r.deferAll(ctx, res, []Outlier{o}, res.Period, false)
	}
	if err := r.Ledger.MarkScheduled(ctx, d, ref); err != nil {
		return fmt.Errorf("record the deep review of %s: %w", o.Database, err)
	}
	res.Scheduled = append(res.Scheduled, d)
	return nil
}

// notIn drops the outliers whose deep review this month was scheduled
// already: a member gets one a month.
func notIn(os []Outlier, done []string) []Outlier {
	skip := make(map[string]bool, len(done))
	for _, d := range done {
		skip[d] = true
	}
	var out []Outlier
	for _, o := range os {
		if !skip[o.Database] {
			out = append(out, o)
		}
	}
	return out
}

// deferAll leaves outliers triaged until period; listed ones are
// reported as deferred.
func (r *Runner) deferAll(ctx context.Context, res *Result, os []Outlier, period string,
	listed bool) error {
	for _, o := range os {
		if err := r.Ledger.MarkDeferred(ctx, o.PassID, o.Database, period); err != nil {
			return fmt.Errorf("defer %s to %s: %w", o.Database, period, err)
		}
		if listed {
			res.Deferred = append(res.Deferred, o.Database)
		}
	}
	return nil
}

// Advance follows a fleet between passes: a member whose deep review ran
// becomes reviewed, and, unless the fleet is stopped, the members waiting
// for this month (after a stop or a refused schedule) are scheduled.
func (r *Runner) Advance(ctx context.Context) (Result, error) {
	res := Result{Period: Period(r.now())}
	if err := r.usable(); err != nil {
		return res, err
	}
	if err := r.markReviewed(ctx, &res); err != nil {
		return res, err
	}
	waiting, err := r.Ledger.Carried(ctx, res.Period)
	if err != nil {
		return res, fmt.Errorf("review fleet: read the members waiting for %s: %w",
			res.Period, err)
	}
	if err := r.schedule(ctx, &res, waiting); err != nil {
		return res, fmt.Errorf("review fleet: %w", err)
	}
	return res, nil
}

func (r *Runner) markReviewed(ctx context.Context, res *Result) error {
	await, err := r.Ledger.AwaitingReview(ctx)
	if err != nil {
		return fmt.Errorf("review fleet: read the scheduled deep reviews: %w", err)
	}
	for _, s := range await {
		done, run, err := r.Deep.DeepStatus(ctx, s.Database, s.Ref)
		if err != nil {
			if res.Failures == nil {
				res.Failures = map[string]string{}
			}
			res.Failures[s.Database] = err.Error()
			continue
		}
		if !done {
			continue
		}
		if err := r.Ledger.MarkReviewed(ctx, s.PassID, s.Database, run); err != nil {
			return fmt.Errorf("review fleet: mark %s reviewed: %w", s.Database, err)
		}
		res.Reviewed++
	}
	return nil
}
