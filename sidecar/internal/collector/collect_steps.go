package collector

import (
	"context"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// catalogStep reads one snapshot category.
type catalogStep struct {
	name string
	run  func(context.Context, *Snapshot) error
}

// catalogSteps lists the catalog categories collect reads, in order. A
// test may override one through overrideStep.
func (c *Collector) catalogSteps() []catalogStep {
	steps := []catalogStep{
		{"queries", func(ctx context.Context, s *Snapshot) (err error) {
			s.Queries, err = c.collectQueries(ctx)
			return err
		}},
		{"tables", func(ctx context.Context, s *Snapshot) (err error) {
			s.Tables, err = c.collectTables(ctx)
			return err
		}},
		{"indexes", func(ctx context.Context, s *Snapshot) (err error) {
			s.Indexes, err = c.collectIndexes(ctx)
			return err
		}},
		{"foreign_keys", func(ctx context.Context, s *Snapshot) (err error) {
			s.ForeignKeys, err = c.collectForeignKeys(ctx)
			return err
		}},
		{"system", func(ctx context.Context, s *Snapshot) (err error) {
			s.System, err = c.collectSystem(ctx)
			return err
		}},
		{"locks", func(ctx context.Context, s *Snapshot) (err error) {
			s.Locks, err = c.collectLocks(ctx)
			return err
		}},
		{"sequences", func(ctx context.Context, s *Snapshot) (err error) {
			s.Sequences, err = c.collectSequences(ctx)
			cov := c.sequenceCoverage()
			s.SequenceCoverage = &cov
			return err
		}},
	}
	for i, st := range steps {
		if fn, ok := c.stepOverrides[st.name]; ok {
			steps[i].run = fn
		}
	}
	return steps
}

// overrideStep replaces one category's reader (tests).
func (c *Collector) overrideStep(name string, fn func(context.Context, *Snapshot) error) {
	if c.stepOverrides == nil {
		c.stepOverrides = map[string]func(context.Context, *Snapshot) error{}
	}
	c.stepOverrides[name] = fn
}

// Retry budget of one collection cycle: a category whose read hit a
// server-side timeout is read once more, and a cycle retries at most
// maxCycleRetries categories (dogfood lifeos, 2026-10-04: the queries and
// indexes reads timed out once in hours and ran in tens of ms after).
const (
	stepAttempts    = 2
	maxCycleRetries = 2
)

// runStep reads one category, once more after a server-side timeout
// while the cycle's retry budget lasts. A permanent failure (a missing
// relation, a permission) is never retried.
func (c *Collector) runStep(ctx context.Context, st catalogStep, snap *Snapshot,
	retries *int) error {
	err := st.run(ctx, snap)
	for attempt := 1; err != nil && attempt < stepAttempts; attempt++ {
		if *retries >= maxCycleRetries || ctx.Err() != nil || !catalogread.Retryable(err) {
			return err
		}
		*retries++
		c.logFn("INFO", "collector: %s timed out (%v); retrying it once this cycle",
			st.name, err)
		err = st.run(ctx, snap)
	}
	return err
}
