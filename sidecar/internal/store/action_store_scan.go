package store

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// scanQueuedActions scans rows into QueuedAction slices.
func scanQueuedActions(rows pgx.Rows) ([]QueuedAction, error) {
	results := []QueuedAction{}
	for rows.Next() {
		a, err := scanQueuedAction(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning queued action: %w", err)
		}
		results = append(results, a)
	}
	return results, rows.Err()
}

// scanQueuedAction scans one row of queuedActionColumns.
func scanQueuedAction(row pgx.Row) (QueuedAction, error) {
	var a QueuedAction
	var rollback *string
	var guardrails []byte
	err := row.Scan(
		&a.ID, &a.DatabaseID, &a.FindingID,
		&a.ProposedSQL, &rollback, &a.ActionRisk,
		&a.Status, &a.ProposedAt, &a.DecidedBy,
		&a.DecidedAt, &a.ExpiresAt, &a.Reason,
		&a.ActionType, &a.IdentityKey, &a.PolicyDecision,
		&guardrails, &a.AttemptCount, &a.LastAttemptAt,
		&a.CooldownUntil, &a.FailureFingerprint,
		&a.LastFailureFingerprint, &a.VerificationStatus,
		&a.ShadowToilMinutes, &a.ActionLogID,
		&a.RecommendationID, &a.RecommendationRevision, &a.ContentHash,
		&a.ProposedVia, &a.ProposedBy,
	)
	if err != nil {
		return a, err
	}
	if rollback != nil {
		a.RollbackSQL = *rollback
	}
	a.Guardrails = decodeGuardrails(guardrails)
	return a, nil
}

func decodeGuardrails(data []byte) []string {
	if len(data) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		return []string{}
	}
	return out
}
