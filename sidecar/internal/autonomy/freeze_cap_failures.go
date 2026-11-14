package autonomy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-05b: the freeze custodian counts the red table's custodian
// VACUUMs the executor's watchdog capped, and from
// freeze.EmergencyCapFailures proposes the emergency profile. This package
// does not import the executor, so it reads the executor's action log.

const (
	// vacuumCapWindow is the executor's custodian backoff window
	// (custodianBackoffWindow): a cap older than it no longer counts.
	vacuumCapWindow = 6 * time.Hour
	// vacuumCapReadTimeout bounds one cap history read.
	vacuumCapReadTimeout = 5 * time.Second
)

// capFailureReader counts a table's capped custodian VACUUMs.
type capFailureReader func(ctx context.Context, schema, table string) (int, error)

// vacuumCapFailuresSQL counts the failed runs of the table's custodian
// VACUUMs whose stored reason starts with the watchdog's vacuum_cap
// outcome, in the window and after the last completed run (which ended
// the streak). The executor stores a failure's error as rollback_reason,
// and a watched VACUUM's error starts with its outcome.
const vacuumCapFailuresSQL = `/* pg_sage freeze custodian */
WITH runs AS (
  SELECT outcome, executed_at, rollback_reason FROM sage.action_log
   WHERE sql_executed = ANY($1::text[])
     AND executed_at > now() - make_interval(secs => $2))
SELECT count(*) FROM runs
 WHERE outcome IN ('failed', 'rollback_failed')
   AND starts_with(COALESCE(rollback_reason, ''), $3)
   AND executed_at > COALESCE((SELECT max(executed_at) FROM runs
                                WHERE outcome IN ('success', 'monitoring')),
                              '-infinity'::timestamptz)`

// readVacuumCapFailures counts schema.table's capped custodian VACUUMs.
func (c *PostgresFreezeCustodian) readVacuumCapFailures(
	ctx context.Context, schema, table string,
) (int, error) {
	if c.pool == nil {
		return 0, errors.New("read vacuum_cap failures: database pool unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, vacuumCapReadTimeout)
	defer cancel()
	var n int
	err := c.pool.QueryRow(ctx, vacuumCapFailuresSQL,
		freeze.CustodianVacuumStatements(schema, table), vacuumCapWindow.Seconds(),
		freeze.VacuumCapOutcome+":").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("read vacuum_cap failures of %s.%s: %w", schema, table, err)
	}
	return n, nil
}

// capFailureCount is the count for a red table's freeze; an unreadable
// history logs one WARN and counts none, so the normal freeze still runs.
func (c *PostgresFreezeCustodian) capFailureCount(
	ctx context.Context, schema, table string,
) int {
	n, err := c.capFailures(ctx, schema, table)
	if err != nil {
		c.logFn("WARN", "freeze custodian: %s.%s keeps the normal VACUUM profile: %v",
			schema, table, err)
		return 0
	}
	return n
}

// WithLogger routes the custodian's warnings (level, format, args).
func (c *PostgresFreezeCustodian) WithLogger(
	fn func(level, format string, args ...any),
) *PostgresFreezeCustodian {
	if fn != nil {
		c.logFn = fn
	}
	return c
}

func stdLogf(level, format string, args ...any) {
	log.Printf("["+level+"] "+format, args...)
}
