package rubric

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StaleRunAfter is how long a run may stay running before the reaper
// fails it: a grade reads a few catalog views and takes seconds.
// failTimeout bounds recording a run whose own context ended.
const (
	StaleRunAfter = 30 * time.Minute
	failTimeout   = 10 * time.Second
)

// Loader reads one database's producer results. A non-nil error with
// usable inputs is a partial read: the parts that failed carry their own
// errors and grade unknown.
type Loader interface {
	Load(ctx context.Context) (Inputs, error)
}

// Runner grades one database and records the run.
type Runner struct {
	Store    *Store
	Loader   Loader
	Rubric   Rubric
	Database string
}

// Outcome is one finished run: its grade, the delta from the previous
// graded run, and the producers that could not be read (LoadErr).
type Outcome struct {
	RunID   int64
	Status  RunStatus
	Grade   Grade
	Delta   Changes
	LoadErr error
}

// Run reaps a stale running row, starts a run (ErrRunning when one is
// running), grades, and finishes it: done, or partial when a producer
// could not be read. A failure to record the run is returned.
func (r *Runner) Run(ctx context.Context, kind Kind, triggers []string) (Outcome, error) {
	if r == nil || r.Loader == nil {
		return Outcome{}, fmt.Errorf("%w: no loader", ErrInvalidRun)
	}
	if _, err := r.Store.Reap(ctx, r.Database, StaleRunAfter); err != nil {
		return Outcome{}, err
	}
	prev, err := r.Store.Latest(ctx, r.Database, 1)
	if err != nil {
		return Outcome{}, err
	}
	id, err := r.Store.Start(ctx, RunSpec{Database: r.Database, Kind: kind,
		Triggers: triggers, RubricVersion: r.Rubric.Version})
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{RunID: id, Status: RunDone}
	in, loadErr := r.Loader.Load(ctx)
	if ctx.Err() != nil {
		return out, r.fail(id, ctx.Err())
	}
	out.Grade = Compute(r.Rubric, Evaluate(r.Rubric, in))
	msg := ""
	if loadErr != nil {
		out.Status, out.LoadErr, msg = RunPartial, loadErr, loadErr.Error()
	}
	if err := r.Store.Finish(ctx, id, out.Status, &out.Grade, msg); err != nil {
		return out, err
	}
	var before *Grade
	if len(prev) > 0 {
		before = &prev[0].Grade
	}
	out.Delta = Delta(before, out.Grade)
	return out, nil
}

// fail records a run whose context ended before it was graded; the
// record uses a fresh context, since the run's own has ended.
func (r *Runner) fail(id int64, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), failTimeout)
	defer cancel()
	if err := r.Store.Finish(ctx, id, RunFailed, nil, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
