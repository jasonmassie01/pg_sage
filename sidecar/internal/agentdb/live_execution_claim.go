package agentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ClaimLiveExecution atomically consumes both authorization and estimate.
// Exactly one caller can win for a server-issued execution tuple.
func (s *Store) ClaimLiveExecution(
	ctx context.Context,
	attempt LiveExecutionAttempt,
	now time.Time,
) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin live execution claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	authTag, err := tx.Exec(ctx, `UPDATE sage.agent_db_live_authorizations
		SET consumed_at=$1
		WHERE authorization_id=$2 AND estimate_id=$3 AND plan_hash=$4
		  AND deployment_id=$5 AND operation=$6 AND requester_id=$7
		  AND idempotency_key=$8 AND consumed_at IS NULL
		  AND revoked_at IS NULL AND expires_at > $1`,
		now, attempt.AuthorizationID, attempt.EstimateID, attempt.PlanHash,
		attempt.DeploymentID, attempt.Operation,
		attempt.AuthenticatedRequesterID, attempt.IdempotencyKey,
	)
	if err != nil {
		return fmt.Errorf("claim live authorization: %w", err)
	}
	if authTag.RowsAffected() != 1 {
		return ErrConflict
	}
	estimateTag, err := tx.Exec(ctx, `UPDATE sage.agent_db_live_estimates
		SET consumed_at=$1
		WHERE estimate_id=$2 AND plan_hash=$3 AND deployment_id=$4
		  AND consumed_at IS NULL AND superseded_at IS NULL AND expires_at > $1`,
		now, attempt.EstimateID, attempt.PlanHash, attempt.DeploymentID,
	)
	if err != nil {
		return fmt.Errorf("claim live estimate: %w", err)
	}
	if estimateTag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit live execution claim: %w", err)
	}
	return nil
}

func (s *Store) PersistLiveExecutionReceipt(
	ctx context.Context,
	receipt LiveExecutionReceipt,
) error {
	if receipt.AuthorizationID == "" || receipt.IdempotencyKey == "" ||
		receipt.PlanHash == "" || receipt.ProviderResourceID == "" {
		return ErrInvalid
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal live receipt: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.agent_db_live_receipts
		(authorization_id, idempotency_key, plan_hash, payload)
		SELECT $1, $2, $3, $4
		  FROM sage.agent_db_live_authorizations
		 WHERE authorization_id=$1 AND idempotency_key=$2 AND plan_hash=$3
		   AND consumed_at IS NOT NULL`,
		receipt.AuthorizationID, receipt.IdempotencyKey,
		receipt.PlanHash, payload,
	)
	if err != nil {
		return liveRecordInsertError("receipt", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: live authorization was not consumed", ErrInvalid)
	}
	return nil
}
