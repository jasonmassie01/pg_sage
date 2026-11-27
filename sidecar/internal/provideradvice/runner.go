package provideradvice

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sink records reconciles (the Store).
type Sink interface {
	Apply(ctx context.Context, database, provider string, rows []Row) error
	RecordSourceError(ctx context.Context, database, provider string, err error) error
}

// RunnerOptions configure one database's advice reconcile.
type RunnerOptions struct {
	Database string
	Sources  []Source
	Sink     Sink
	Findings func(ctx context.Context) ([]Finding, error)
	Eval     Evaluator
}

// Runner reconciles each source's advice for one database.
type Runner struct{ opts RunnerOptions }

// NewRunner validates the options: every part is required and each
// provider appears once.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Database == "" || len(opts.Sources) == 0 || opts.Sink == nil ||
		opts.Findings == nil || opts.Eval == nil {
		return nil, errors.New("provider advice needs a database, sources, a store, " +
			"findings and an evaluator")
	}
	seen := map[string]bool{}
	for _, s := range opts.Sources {
		if s == nil || s.Provider() == "" || seen[s.Provider()] {
			return nil, errors.New("provider advice sources must be distinct providers")
		}
		seen[s.Provider()] = true
	}
	return &Runner{opts: opts}, nil
}

// RunOnce reads pg_sage's findings, then fetches and reconciles each
// source. A source that fails records its error and keeps its earlier
// advice; the others still apply. The error joins every failure.
func (r *Runner) RunOnce(ctx context.Context) error {
	findings, err := r.opts.Findings(ctx)
	if err != nil {
		return fmt.Errorf("provider advice of %s: %w", r.opts.Database, err)
	}
	var errs []error
	for _, src := range r.opts.Sources {
		if err := r.reconcile(ctx, src, findings); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) reconcile(ctx context.Context, src Source, findings []Finding) error {
	db, provider := r.opts.Database, src.Provider()
	recs, err := src.Fetch(ctx)
	if err != nil {
		fetchErr := fmt.Errorf("%s advice of %s: %w", provider, db, err)
		if recErr := r.opts.Sink.RecordSourceError(ctx, db, provider, err); recErr != nil {
			return errors.Join(fetchErr, recErr)
		}
		return fetchErr
	}
	rows := Reconcile(ctx, recs, findings, r.opts.Eval)
	if err := r.opts.Sink.Apply(ctx, db, provider, rows); err != nil {
		return fmt.Errorf("%s advice of %s: %w", provider, db, err)
	}
	return nil
}

// Run reconciles at once and then every interval until ctx ends; report
// (required) receives each failed run's error.
func (r *Runner) Run(ctx context.Context, interval time.Duration, report func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
