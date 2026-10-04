package tuning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// settingActionsSQL reads the configuration and storage-parameter changes
// since $1 (idx_action_log_time) with their verification verdicts.
const settingActionsSQL = `/* pg_sage */
SELECT a.id, a.executed_at, a.sql_executed, COALESCE(a.rollback_sql, ''), a.outcome,
       COALESCE(o.verdict, ''), o.decided_at
FROM sage.action_log a
LEFT JOIN sage.action_outcome o ON o.action_log_id = a.id
WHERE a.executed_at >= $1
  AND (a.sql_executed ILIKE 'ALTER SYSTEM%' OR a.sql_executed ILIKE 'ALTER TABLE%')
ORDER BY a.executed_at, a.id`

func (s *postgresStore) SettingActions(ctx context.Context, since time.Time) (
	[]SettingAction, error) {
	rows, err := s.pool.Query(ctx, settingActionsSQL, since)
	if err != nil {
		return nil, fmt.Errorf("read setting changes: %w", err)
	}
	defer rows.Close()
	var ledger []ledgerAction
	for rows.Next() {
		var r ledgerAction
		if err := rows.Scan(&r.ID, &r.ExecutedAt, &r.SQL, &r.Rollback, &r.Outcome,
			&r.Verdict, &r.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan setting change: %w", err)
		}
		ledger = append(ledger, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read setting changes: %w", err)
	}
	return settingActionsFrom(ledger), nil
}

const dayBudgetUsedSQL = `/* pg_sage */
SELECT tokens_used, requests_used FROM sage.tuning_budget_day
WHERE database_name = current_database() AND utc_day = $1::date`

const chargeDayBudgetSQL = `/* pg_sage */
INSERT INTO sage.tuning_budget_day AS b (utc_day, tokens_used, requests_used)
VALUES ($1::date, $2, $3)
ON CONFLICT (database_name, utc_day) DO UPDATE
SET tokens_used = b.tokens_used + EXCLUDED.tokens_used,
    requests_used = b.requests_used + EXCLUDED.requests_used, updated_at = now()`

func (s *postgresStore) DayBudgetUsed(ctx context.Context, day time.Time) (int64, int64,
	error) {
	var tokens, requests int64
	err := s.pool.QueryRow(ctx, dayBudgetUsedSQL, utcDay(day)).Scan(&tokens, &requests)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read the tuning budget of %s: %w", utcDay(day), err)
	}
	return tokens, requests, nil
}

func (s *postgresStore) ChargeDayBudget(ctx context.Context, day time.Time, tokens,
	requests int64) error {
	if tokens < 0 || requests < 0 {
		return fmt.Errorf("charge the tuning budget: negative spend %d tokens, %d requests",
			tokens, requests)
	}
	if _, err := s.pool.Exec(ctx, chargeDayBudgetSQL, utcDay(day), tokens,
		requests); err != nil {
		return fmt.Errorf("charge the tuning budget of %s: %w", utcDay(day), err)
	}
	return nil
}

// utcDay is the UTC calendar day of t.
func utcDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }
