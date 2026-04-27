package store

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// scanQueuedActions scans rows into QueuedAction slices.
func scanQueuedActions(rows pgx.Rows) ([]QueuedAction, error) {
	var results []QueuedAction
	for rows.Next() {
		var a QueuedAction
		var rollback *string
		var guardrails []byte
		err := rows.Scan(
			&a.ID, &a.DatabaseID, &a.FindingID,
			&a.ProposedSQL, &rollback, &a.ActionRisk,
			&a.Status, &a.ProposedAt, &a.DecidedBy,
			&a.DecidedAt, &a.ExpiresAt, &a.Reason,
			&a.ActionType, &a.IdentityKey, &a.PolicyDecision,
			&guardrails, &a.AttemptCount, &a.LastAttemptAt,
			&a.CooldownUntil, &a.FailureFingerprint,
			&a.LastFailureFingerprint, &a.VerificationStatus,
			&a.ShadowToilMinutes,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning queued action: %w", err)
		}
		if rollback != nil {
			a.RollbackSQL = *rollback
		}
		a.Guardrails = decodeGuardrails(guardrails)
		results = append(results, a)
	}
	if results == nil {
		results = []QueuedAction{}
	}
	return results, rows.Err()
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
