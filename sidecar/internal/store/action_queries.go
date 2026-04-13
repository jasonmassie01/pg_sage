package store

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

const recentSageActionsSQL = `
SELECT id, action_type, executed_at, sql_executed,
       outcome, rollback_reason, measured_at
FROM sage.action_log
WHERE executed_at > now() - $1::interval
ORDER BY executed_at DESC`

const rollbackHistorySQL = `
SELECT id, action_type, executed_at, sql_executed,
       outcome, rollback_reason, measured_at
FROM sage.action_log
WHERE outcome = 'rolled_back'
  AND measured_at > now() - $1::interval
ORDER BY measured_at DESC`

// RecentSageActions returns actions from sage.action_log within the
// given lookback window. Used by RCA self-action correlation.
func (s *ActionStore) RecentSageActions(
	ctx context.Context,
	lookback time.Duration,
) ([]rca.SageAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx, recentSageActionsSQL,
		fmt.Sprintf("%d seconds", int(lookback.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("querying recent sage actions: %w", err)
	}
	defer rows.Close()

	return scanSageActions(rows)
}

// RollbackHistory returns actions that were rolled back within the
// given lookback period. Used for anti-oscillation detection.
func (s *ActionStore) RollbackHistory(
	ctx context.Context,
	lookback time.Duration,
) ([]rca.SageAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx, rollbackHistorySQL,
		fmt.Sprintf("%d seconds", int(lookback.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("querying rollback history: %w", err)
	}
	defer rows.Close()

	return scanSageActions(rows)
}

// scanSageActions scans rows into rca.SageAction slices. Maps the
// action_log schema to the SageAction struct:
//   - action_type  -> Family
//   - sql_executed -> Description
//   - outcome='rolled_back' -> RolledBack
//   - measured_at (when rolled back) -> RolledBackAt
func scanSageActions(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]rca.SageAction, error) {
	var results []rca.SageAction
	for rows.Next() {
		var (
			id             int64
			actionType     string
			executedAt     time.Time
			sqlExecuted    string
			outcome        string
			rollbackReason *string
			measuredAt     *time.Time
		)
		err := rows.Scan(
			&id, &actionType, &executedAt, &sqlExecuted,
			&outcome, &rollbackReason, &measuredAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning sage action: %w", err)
		}

		a := rca.SageAction{
			ID:          fmt.Sprintf("%d", id),
			Family:      actionType,
			ExecutedAt:  executedAt,
			Description: sqlExecuted,
			RolledBack:  outcome == "rolled_back",
		}
		if a.RolledBack && measuredAt != nil {
			a.RolledBackAt = measuredAt
		}
		results = append(results, a)
	}
	if results == nil {
		results = []rca.SageAction{}
	}
	return results, rows.Err()
}
