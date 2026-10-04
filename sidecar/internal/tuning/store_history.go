package tuning

import (
	"context"
	"fmt"
	"time"
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
