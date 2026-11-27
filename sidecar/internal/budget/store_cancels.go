package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CancelRow is the cancels to add to one budget definition's ledger day.
type CancelRow struct {
	Budget, Definition string
	Day                time.Time
	Count              int64
	LowerBound         bool
}

// CancelLedger is the ledger's record of real cancels and of reverts
// (Store). A Ledger without it records no cancels and sees no reverts.
type CancelLedger interface {
	// RecordCancels adds rows to the ledger and marks today's rows with
	// where their cancels came from (CancelSource*).
	RecordCancels(ctx context.Context, today time.Time, source string, rows []CancelRow) error
	// LastRevert is when the latest rollback of the enforce statement sql
	// was recorded (zero: never).
	LastRevert(ctx context.Context, sql string) (time.Time, error)
}

// recordCancelSQL adds a row's cancels to its day, or to today's row when
// pg_sage did not measure that day (a row is never invented for it: a
// measured day counts toward promotion).
const recordCancelSQL = `UPDATE /* pg_sage budget */ sage.budget_shadow
   SET actual_cancels = actual_cancels + $5, cancel_source = $6,
       cancels_lower_bound = cancels_lower_bound OR $7, updated_at = now()
 WHERE database_name = $1 AND budget = $2 AND definition = $3
   AND day = CASE WHEN EXISTS (SELECT 1 FROM sage.budget_shadow
                                WHERE database_name = $1 AND budget = $2
                                  AND definition = $3 AND day = $4::date)
                  THEN $4::date ELSE $8::date END`

// RecordCancels adds the rows' cancels and sets today's cancel_source of
// this database's rows to source.
func (s *Store) RecordCancels(ctx context.Context, today time.Time, source string,
	rows []CancelRow) error {
	batch := &pgx.Batch{}
	day := utcDay(today)
	batch.Queue(`UPDATE /* pg_sage budget */ sage.budget_shadow SET cancel_source = $3
 WHERE database_name = $1 AND day = $2::date AND cancel_source <> $3`, s.database, day,
		source)
	for _, r := range rows {
		batch.Queue(recordCancelSQL, s.database, r.Budget, r.Definition, utcDay(r.Day),
			r.Count, source, r.LowerBound, day)
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("update sage.budget_shadow cancels: %w", err)
	}
	return nil
}

// LastRevert is when the latest rollback of sql (a set_role_timeout
// statement) was recorded: an automatic revert, an operator rollback, or
// one that failed or was withheld (the timeout was judged wrong either
// way). Zero when there was none.
func (s *Store) LastRevert(ctx context.Context, sql string) (time.Time, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `/* pg_sage budget */
SELECT max(COALESCE(measured_at, executed_at))
  FROM sage.action_log
 WHERE sql_executed = $1
   AND outcome IN ('rolling_back', 'rolled_back', 'rollback_failed', 'rollback_skipped')`,
		sql).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("read reverts from sage.action_log: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return at.UTC(), nil
}
