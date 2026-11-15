package collector

import (
	"context"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// catalogStep reads one snapshot category. reset, set on the extra
// categories, drops a partial reading when the read fails, so the field
// is unknown (nil) rather than a half-read value.
type catalogStep struct {
	name  string
	run   func(context.Context, *Snapshot) error
	reset func(*Snapshot)
}

// catalogSteps lists the catalog categories collect reads, in order. A
// test may override one through overrideStep.
func (c *Collector) catalogSteps() []catalogStep {
	steps := []catalogStep{
		{name: "queries", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.Queries, err = c.queriesVia(ctx, timedCatalogQuerier{collector: c},
				&s.stmtTotals)
			return err
		}},
		{name: "tables", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.Tables, err = c.collectTables(ctx)
			return err
		}},
		{name: "indexes", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.Indexes, err = c.collectIndexes(ctx)
			return err
		}},
		{name: "partitioned_indexes", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.PartitionedIndexes, err = c.collectPartitionedIndexes(ctx)
			return err
		}},
		{name: "foreign_keys", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.ForeignKeys, err = c.collectForeignKeys(ctx)
			return err
		}},
		{name: "system", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.System, err = c.collectSystem(ctx)
			return err
		}},
		{name: "locks", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.Locks, s.LocksDropped, err = c.collectLocks(ctx)
			return err
		}},
		{name: "sequences", run: func(ctx context.Context, s *Snapshot) (err error) {
			s.Sequences, err = c.collectSequences(ctx)
			cov := c.sequenceCoverage()
			s.SequenceCoverage = &cov
			return err
		}},
	}
	return c.withOverrides(steps)
}

// extraSteps lists the categories read after the catalog. They share the
// cycle's retry budget; one that fails is unknown, not empty: its field is
// cleared, it is marked unavailable and the snapshot goes on (collection-05).
// io is read from PG16 (pg_stat_io) on, cdc_heartbeats only when some are
// configured and config_data only for an advisor consumer: a category not
// read is not collected, which is not failed.
func (c *Collector) extraSteps() []catalogStep {
	var steps []catalogStep
	if c.Capabilities().PGVersionNum >= 160000 {
		steps = append(steps, catalogStep{name: "io",
			run: func(ctx context.Context, s *Snapshot) (err error) {
				s.IO, err = c.collectIO(ctx)
				return err
			}, reset: func(s *Snapshot) { s.IO = nil }})
	}
	// Prepared transactions (2PC): invisible to pg_stat_activity, they
	// hold xmin and locks indefinitely.
	steps = append(steps, catalogStep{name: "prepared_xacts",
		run: func(ctx context.Context, s *Snapshot) (err error) {
			s.PreparedXacts, err = c.collectPreparedXacts(ctx)
			return err
		}, reset: func(s *Snapshot) { s.PreparedXacts = nil }},
		catalogStep{name: "archiver",
			run: func(ctx context.Context, s *Snapshot) (err error) {
				s.Archiver, err = c.collectArchiver(ctx)
				return err
			}, reset: func(s *Snapshot) { s.Archiver = nil }},
		catalogStep{name: "partitions",
			run: func(ctx context.Context, s *Snapshot) (err error) {
				s.Partitions, err = c.collectPartitions(ctx)
				return err
			}, reset: func(s *Snapshot) { s.Partitions = nil }},
		catalogStep{name: "index_bloat",
			run: func(ctx context.Context, s *Snapshot) (err error) {
				var gaps indexBloatGaps
				s.IndexBloat, gaps, err = c.collectIndexBloat(ctx)
				s.IndexBloatTruncated, s.IndexBloatUnknown = gaps.truncated, gaps.unknown
				return err
			}, reset: func(s *Snapshot) {
				s.IndexBloat, s.IndexBloatTruncated, s.IndexBloatUnknown = nil, false, nil
			}})
	steps = append(steps, c.sessionSteps()...)
	steps = append(steps, c.replicationHealthSteps()...)
	if c.CollectsConfigSnapshots() {
		steps = append(steps, catalogStep{name: "config_data",
			run: func(ctx context.Context, s *Snapshot) (err error) {
				s.ConfigData, err = collectConfigSnapshot(ctx, timedCatalogQuerier{collector: c})
				return err
			}, reset: func(s *Snapshot) { s.ConfigData = nil }})
	}
	return c.withOverrides(steps)
}

// sessionSteps read the client sessions: workload_sessions by role,
// application and state for workload budgets (market-08), then one aggregate
// of the active sessions by wait class (market-01).
func (c *Collector) sessionSteps() []catalogStep {
	return []catalogStep{{name: "workload_sessions",
		run: func(ctx context.Context, s *Snapshot) (err error) {
			s.WorkloadSessions, err = c.collectWorkloadSessions(ctx)
			return err
		}, reset: func(s *Snapshot) { s.WorkloadSessions = nil }},
		{name: "wait_events",
			run: func(ctx context.Context, s *Snapshot) (err error) {
				s.WaitEvents, err = c.collectWaitEvents(ctx)
				return err
			}, reset: func(s *Snapshot) { s.WaitEvents = nil }}}
}

// withOverrides swaps in the readers a test installed through overrideStep.
func (c *Collector) withOverrides(steps []catalogStep) []catalogStep {
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
	ctx = withStep(ctx, st.name)
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
