package advisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgRunStore keeps the schedule in sage.advisor_runs, one row per
// database and sub-advisor, so a restart within the interval makes no
// model call and two concurrent cycles make one.
type pgRunStore struct{ pool *pgxpool.Pool }

const loadRunsSQL = `/* pg_sage */
SELECT sub_advisor, last_success_at, consecutive_failures, next_attempt_at
FROM sage.advisor_runs
WHERE database_name = current_database()`

func (s pgRunStore) LoadRuns(ctx context.Context) (map[string]runState, error) {
	rows, err := s.pool.Query(ctx, loadRunsSQL)
	if err != nil {
		return nil, fmt.Errorf("read sage.advisor_runs: %w", err)
	}
	defer rows.Close()
	out := map[string]runState{}
	for rows.Next() {
		var name string
		var last, next *time.Time
		var failures int
		if err := rows.Scan(&name, &last, &failures, &next); err != nil {
			return nil, fmt.Errorf("scan sage.advisor_runs: %w", err)
		}
		out[name] = runState{LastSuccessAt: deref(last), ConsecutiveFailures: failures,
			NextAttemptAt: deref(next)}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sage.advisor_runs: %w", err)
	}
	return out, nil
}

// claimRunSQL takes the row lock: a concurrent claim waits, then re-reads
// next_attempt_at (now the lease) and claims nothing.
const claimRunSQL = `/* pg_sage */
INSERT INTO sage.advisor_runs AS r (sub_advisor, next_attempt_at, updated_at)
VALUES ($1, $3, now())
ON CONFLICT (database_name, sub_advisor) DO UPDATE
   SET next_attempt_at = EXCLUDED.next_attempt_at, updated_at = now()
 WHERE r.next_attempt_at IS NULL OR r.next_attempt_at <= $2
RETURNING r.sub_advisor`

func (s pgRunStore) ClaimRun(ctx context.Context, name string, now,
	leaseUntil time.Time) (bool, error) {
	var got string
	err := s.pool.QueryRow(ctx, claimRunSQL, name, now, leaseUntil).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim sage.advisor_runs %s: %w", name, err)
	}
	return true, nil
}

const saveRunSQL = `/* pg_sage */
INSERT INTO sage.advisor_runs AS r
       (sub_advisor, last_success_at, consecutive_failures, next_attempt_at, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (database_name, sub_advisor) DO UPDATE
   SET last_success_at = EXCLUDED.last_success_at,
       consecutive_failures = EXCLUDED.consecutive_failures,
       next_attempt_at = EXCLUDED.next_attempt_at,
       updated_at = now()`

func (s pgRunStore) SaveRun(ctx context.Context, name string, st runState) error {
	_, err := s.pool.Exec(ctx, saveRunSQL, name, nullTime(st.LastSuccessAt),
		st.ConsecutiveFailures, nullTime(st.NextAttemptAt))
	if err != nil {
		return fmt.Errorf("save sage.advisor_runs %s: %w", name, err)
	}
	return nil
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// nullTime stores the zero time as NULL.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
