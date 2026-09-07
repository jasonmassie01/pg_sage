package agentdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PersistLiveExecutionRecords stores an immutable server-issued execution
// tuple. Existing identities cannot be overwritten with broader claims.
func (s *Store) PersistLiveExecutionRecords(
	ctx context.Context,
	records LiveExecutionRecords,
) error {
	if err := validatePersistedLiveRecords(records); err != nil {
		return err
	}
	planJSON, err := json.Marshal(records.Plan)
	if err != nil {
		return fmt.Errorf("marshal live plan: %w", err)
	}
	estimateJSON, err := json.Marshal(records.Estimate)
	if err != nil {
		return fmt.Errorf("marshal live estimate: %w", err)
	}
	authJSON, err := json.Marshal(records.Authorization)
	if err != nil {
		return fmt.Errorf("marshal live authorization: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin live record persistence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertLivePlan(ctx, tx, records, planJSON); err != nil {
		return err
	}
	if err := insertLiveEstimate(ctx, tx, records, estimateJSON); err != nil {
		return err
	}
	if err := insertLiveAuthorization(ctx, tx, records, authJSON); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit live records: %w", err)
	}
	return nil
}

func validatePersistedLiveRecords(records LiveExecutionRecords) error {
	if records.Plan == nil || records.Estimate == nil || records.Authorization == nil {
		return ErrInvalid
	}
	attempt := LiveExecutionAttempt{
		DeploymentID:             records.Plan.DeploymentID,
		Provider:                 records.Plan.Provider,
		Operation:                records.Plan.Operation,
		PlanHash:                 records.Plan.Hash,
		EstimateID:               records.Estimate.EstimateID,
		AuthorizationID:          records.Authorization.AuthorizationID,
		AuthenticatedRequesterID: records.Authorization.RequesterID,
		IdempotencyKey:           records.Authorization.IdempotencyKey,
	}
	result := ValidateLiveExecutionAttempt(records, attempt, time.Now().UTC())
	if !result.Allowed {
		return fmt.Errorf("%w: invalid live execution records", ErrInvalid)
	}
	return nil
}

func insertLivePlan(
	ctx context.Context,
	tx pgx.Tx,
	records LiveExecutionRecords,
	payload []byte,
) error {
	tag, err := tx.Exec(ctx, `INSERT INTO sage.agent_db_live_plans
		(plan_hash, deployment_id, payload) VALUES ($1, $2, $3)
		ON CONFLICT (plan_hash) DO NOTHING`,
		records.Plan.Hash, records.Plan.DeploymentID, payload,
	)
	if err != nil {
		return liveRecordInsertError("plan", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exact bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM sage.agent_db_live_plans
		WHERE plan_hash=$1 AND deployment_id=$2 AND payload=$3::jsonb)`,
		records.Plan.Hash, records.Plan.DeploymentID, payload,
	).Scan(&exact)
	if err != nil {
		return fmt.Errorf("verify immutable live plan: %w", err)
	}
	if !exact {
		return fmt.Errorf("%w: live plan identity changed", ErrConflict)
	}
	return nil
}

func insertLiveEstimate(
	ctx context.Context,
	tx pgx.Tx,
	records LiveExecutionRecords,
	payload []byte,
) error {
	estimate := records.Estimate
	_, err := tx.Exec(ctx, `INSERT INTO sage.agent_db_live_estimates
		(estimate_id, plan_hash, deployment_id, payload, expires_at, superseded_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		estimate.EstimateID, estimate.PlanHash, estimate.DeploymentID,
		payload, estimate.ExpiresAt, estimate.SupersededAt,
	)
	return liveRecordInsertError("estimate", err)
}

func insertLiveAuthorization(
	ctx context.Context,
	tx pgx.Tx,
	records LiveExecutionRecords,
	payload []byte,
) error {
	authz := records.Authorization
	_, err := tx.Exec(ctx, `INSERT INTO sage.agent_db_live_authorizations
		(authorization_id, estimate_id, plan_hash, deployment_id, operation,
		 requester_id, idempotency_key, payload, expires_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		authz.AuthorizationID, authz.EstimateID, authz.PlanHash,
		authz.DeploymentID, authz.Operation, authz.RequesterID,
		authz.IdempotencyKey, payload, authz.ExpiresAt, authz.RevokedAt,
	)
	return liveRecordInsertError("authorization", err)
}

func liveRecordInsertError(kind string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: live %s already exists", ErrConflict, kind)
	}
	return fmt.Errorf("persist live %s: %w", kind, err)
}

func (s *Store) FindLiveExecutionAttempt(
	ctx context.Context,
	deploymentID string,
	operation ProvisionOperation,
	requesterID string,
	idempotencyKey string,
) (LiveExecutionAttempt, error) {
	attempt := LiveExecutionAttempt{
		DeploymentID: deploymentID, Operation: operation,
		AuthenticatedRequesterID: requesterID, IdempotencyKey: idempotencyKey,
	}
	err := s.pool.QueryRow(ctx, `SELECT a.plan_hash, a.estimate_id,
		a.authorization_id, p.payload->>'provider'
		FROM sage.agent_db_live_authorizations a
		JOIN sage.agent_db_live_plans p ON p.plan_hash=a.plan_hash
		WHERE a.deployment_id=$1 AND a.operation=$2
		  AND a.requester_id=$3 AND a.idempotency_key=$4`,
		deploymentID, operation, requesterID, idempotencyKey,
	).Scan(
		&attempt.PlanHash, &attempt.EstimateID,
		&attempt.AuthorizationID, &attempt.Provider,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return LiveExecutionAttempt{}, ErrNotFound
	}
	if err != nil {
		return LiveExecutionAttempt{}, fmt.Errorf("find live execution attempt: %w", err)
	}
	return attempt, nil
}

func (s *Store) LiveExecutionAttemptByAuthorization(
	ctx context.Context,
	authorizationID string,
) (LiveExecutionAttempt, error) {
	attempt := LiveExecutionAttempt{AuthorizationID: authorizationID}
	err := s.pool.QueryRow(ctx, `SELECT a.deployment_id, p.payload->>'provider',
		a.operation, a.plan_hash, a.estimate_id, a.requester_id,
		a.idempotency_key
		FROM sage.agent_db_live_authorizations a
		JOIN sage.agent_db_live_plans p ON p.plan_hash=a.plan_hash
		WHERE a.authorization_id=$1`, authorizationID,
	).Scan(
		&attempt.DeploymentID, &attempt.Provider, &attempt.Operation,
		&attempt.PlanHash, &attempt.EstimateID,
		&attempt.AuthenticatedRequesterID, &attempt.IdempotencyKey,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return LiveExecutionAttempt{}, ErrNotFound
	}
	if err != nil {
		return LiveExecutionAttempt{}, fmt.Errorf("load live authorization identity: %w", err)
	}
	return attempt, nil
}

// LoadLiveExecutionRecords loads only records matching the request tuple.
// Current policy and pricing provenance come from trusted runtime state.
func (s *Store) LoadLiveExecutionRecords(
	ctx context.Context,
	attempt LiveExecutionAttempt,
	current LiveExecutionRecords,
) (LiveExecutionRecords, error) {
	loaded := LiveExecutionRecords{
		CurrentPolicy:          current.CurrentPolicy,
		CurrentPolicyHash:      current.CurrentPolicyHash,
		CurrentPolicyVersion:   current.CurrentPolicyVersion,
		CurrentPricingRevision: current.CurrentPricingRevision,
	}
	plan, err := s.loadLivePlan(ctx, attempt)
	if err != nil {
		return loaded, err
	}
	loaded.Plan = plan
	estimate, err := s.loadLiveEstimate(ctx, attempt)
	if err != nil {
		return loaded, err
	}
	loaded.Estimate = estimate
	authz, err := s.loadLiveAuthorization(ctx, attempt)
	if err != nil {
		return loaded, err
	}
	loaded.Authorization = authz
	receipt, err := s.loadLiveReceipt(ctx, attempt.AuthorizationID)
	if err != nil {
		return loaded, err
	}
	loaded.Receipt = receipt
	return loaded, nil
}

func (s *Store) loadLivePlan(
	ctx context.Context,
	attempt LiveExecutionAttempt,
) (*NormalizedLivePlan, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT payload
		FROM sage.agent_db_live_plans
		WHERE plan_hash=$1 AND deployment_id=$2`,
		attempt.PlanHash, attempt.DeploymentID,
	).Scan(&payload)
	if err != nil {
		return nil, liveRecordLoadError("plan", err)
	}
	var plan NormalizedLivePlan
	if err := json.Unmarshal(payload, &plan); err != nil {
		return nil, fmt.Errorf("decode live plan: %w", err)
	}
	return &plan, nil
}

func (s *Store) loadLiveEstimate(
	ctx context.Context,
	attempt LiveExecutionAttempt,
) (*IssuedLiveCostEstimate, error) {
	var payload []byte
	var consumedAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT payload, consumed_at
		FROM sage.agent_db_live_estimates
		WHERE estimate_id=$1 AND plan_hash=$2 AND deployment_id=$3`,
		attempt.EstimateID, attempt.PlanHash, attempt.DeploymentID,
	).Scan(&payload, &consumedAt)
	if err != nil {
		return nil, liveRecordLoadError("estimate", err)
	}
	var estimate IssuedLiveCostEstimate
	if err := json.Unmarshal(payload, &estimate); err != nil {
		return nil, fmt.Errorf("decode live estimate: %w", err)
	}
	estimate.ConsumedAt = consumedAt
	return &estimate, nil
}

func (s *Store) loadLiveAuthorization(
	ctx context.Context,
	attempt LiveExecutionAttempt,
) (*LiveOperationAuthorization, error) {
	var payload []byte
	var consumedAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT payload, consumed_at
		FROM sage.agent_db_live_authorizations
		WHERE authorization_id=$1 AND estimate_id=$2 AND plan_hash=$3
		  AND deployment_id=$4 AND operation=$5 AND requester_id=$6
		  AND idempotency_key=$7`,
		attempt.AuthorizationID, attempt.EstimateID, attempt.PlanHash,
		attempt.DeploymentID, attempt.Operation,
		attempt.AuthenticatedRequesterID, attempt.IdempotencyKey,
	).Scan(&payload, &consumedAt)
	if err != nil {
		return nil, liveRecordLoadError("authorization", err)
	}
	var authz LiveOperationAuthorization
	if err := json.Unmarshal(payload, &authz); err != nil {
		return nil, fmt.Errorf("decode live authorization: %w", err)
	}
	authz.ConsumedAt = consumedAt
	return &authz, nil
}

func (s *Store) loadLiveReceipt(
	ctx context.Context,
	authorizationID string,
) (*LiveExecutionReceipt, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT payload
		FROM sage.agent_db_live_receipts WHERE authorization_id=$1`,
		authorizationID,
	).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load live receipt: %w", err)
	}
	var receipt LiveExecutionReceipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return nil, fmt.Errorf("decode live receipt: %w", err)
	}
	return &receipt, nil
}

func liveRecordLoadError(kind string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: live %s does not match", ErrInvalid, kind)
	}
	return fmt.Errorf("load live %s: %w", kind, err)
}
