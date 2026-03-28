package store

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// scanQueuedActions scans rows into QueuedAction slices.
func scanQueuedActions(rows pgx.Rows) ([]QueuedAction, error) {
	var results []QueuedAction
	for rows.Next() {
		var a QueuedAction
		var rollback *string
		err := rows.Scan(
			&a.ID, &a.DatabaseID, &a.FindingID,
			&a.ProposedSQL, &rollback, &a.ActionRisk,
			&a.Status, &a.ProposedAt, &a.DecidedBy,
			&a.DecidedAt, &a.ExpiresAt, &a.Reason,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning queued action: %w", err)
		}
		if rollback != nil {
			a.RollbackSQL = *rollback
		}
		results = append(results, a)
	}
	if results == nil {
		results = []QueuedAction{}
	}
	return results, rows.Err()
}
