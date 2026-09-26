package collector

import (
	"context"
	"time"
)

// statementsInfoProbeSQL reports whether pg_stat_statements_info exists
// (pg_stat_statements 1.9+, shipped with PostgreSQL 14).
const statementsInfoProbeSQL = sageTag +
	`SELECT to_regclass('pg_stat_statements_info') IS NOT NULL`

// statementsEpochSQL returns the instant since which pg_stat_statements
// counters have been accumulating: the later of the last full
// pg_stat_statements_reset() and the postmaster start (a crash discards
// the counters without touching stats_reset).
const statementsEpochSQL = sageTag + `
SELECT GREATEST(COALESCE(i.stats_reset, '-infinity'::timestamptz),
                pg_postmaster_start_time())
  FROM pg_stat_statements_info i`

// postmasterEpochSQL is the epoch when pg_stat_statements_info is absent:
// only a server restart can be detected.
const postmasterEpochSQL = sageTag + `SELECT pg_postmaster_start_time()`

// collectStatementsEpoch returns the pg_stat_statements statistics epoch,
// or the zero time when it cannot be read. Counters from different epochs
// are not comparable, even when they have regrown past an earlier sample
// (R10). An unknown epoch is recorded as such, never guessed.
func (c *Collector) collectStatementsEpoch(ctx context.Context) time.Time {
	var hasInfo bool
	if err := c.catalogQueryRow(ctx, statementsInfoProbeSQL).Scan(&hasInfo); err != nil {
		c.logFn("WARN", "probe pg_stat_statements_info: %v; "+
			"statistics epoch unknown this cycle", err)
		return time.Time{}
	}
	sql := postmasterEpochSQL
	if hasInfo {
		sql = statementsEpochSQL
	}
	var epoch time.Time
	if err := c.catalogQueryRow(ctx, sql).Scan(&epoch); err != nil {
		c.logFn("WARN", "read pg_stat_statements statistics epoch: %v; "+
			"statistics epoch unknown this cycle", err)
		return time.Time{}
	}
	return epoch
}

// epochChanged reports whether two known statistics epochs differ. An
// unknown (zero) epoch on either side is not evidence of a reset here;
// the query store treats unknown epochs as incomparable on its own.
func epochChanged(previous, current time.Time) bool {
	return !previous.IsZero() && !current.IsZero() && !previous.Equal(current)
}
