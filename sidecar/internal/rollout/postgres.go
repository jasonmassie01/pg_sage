package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRunStore struct {
	pool *pgxpool.Pool
}

func NewPostgresRunStore(pool *pgxpool.Pool) *PostgresRunStore {
	return &PostgresRunStore{pool: pool}
}

func (store *PostgresRunStore) Create(ctx context.Context, record RunRecord) error {
	if store == nil || store.pool == nil {
		return errors.New("rollout run store is unavailable")
	}
	policy, err := json.Marshal(record.Policy)
	if err != nil {
		return fmt.Errorf("encode rollout policy: %w", err)
	}
	targets, err := json.Marshal(append([]string{}, record.Targets...))
	if err != nil {
		return fmt.Errorf("encode rollout targets: %w", err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO sage.rollout_run
		(evidence_id, source_instance, prior_evidence_id, policy, state,
		 aggregate_regression_pct, applied_instances, created_at, updated_at,
		 halt_reason, family, action_class, started_by, targets)
		VALUES ($1, $2, $3, $4, $5,
		 NULLIF($6::double precision, 0)::numeric, $7, $8, $9,
		 NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), $14)`,
		record.EvidenceID, record.SourceInstance, record.PriorEvidenceID, policy,
		record.State, record.AggregateRegressionPct, record.AppliedInstances,
		record.CreatedAt, record.UpdatedAt, record.HaltReason, record.Family,
		record.Class, record.StartedBy, targets)
	if err != nil {
		return fmt.Errorf("insert rollout run: %w", err)
	}
	return nil
}

// Update records a run's progress. A run another sidecar marked
// interrupted stays interrupted: Update returns ErrRunInterrupted.
func (store *PostgresRunStore) Update(ctx context.Context, record RunRecord) error {
	if store == nil || store.pool == nil {
		return errors.New("rollout run store is unavailable")
	}
	policy, err := json.Marshal(record.Policy)
	if err != nil {
		return fmt.Errorf("encode rollout policy: %w", err)
	}
	tag, err := store.pool.Exec(ctx, `UPDATE sage.rollout_run SET
		source_instance=$2, prior_evidence_id=$3, policy=$4, state=$5,
		aggregate_regression_pct=NULLIF($6::double precision, 0)::numeric,
		applied_instances=$7,
		updated_at=$8, halt_reason=NULLIF($9, ''), family=NULLIF($10, ''),
		action_class=NULLIF($11, ''), started_by=NULLIF($12, '')
		WHERE evidence_id=$1 AND state <> 'interrupted'`,
		record.EvidenceID, record.SourceInstance, record.PriorEvidenceID, policy,
		record.State, record.AggregateRegressionPct, record.AppliedInstances,
		record.UpdatedAt, record.HaltReason, record.Family, record.Class, record.StartedBy)
	if err != nil {
		return fmt.Errorf("update rollout run: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if run, err := store.Get(ctx, record.EvidenceID); err == nil &&
		run.State == StateInterrupted {
		return fmt.Errorf("record rollout run %s: %w", record.EvidenceID, ErrRunInterrupted)
	}
	return fmt.Errorf("rollout run %q not found", record.EvidenceID)
}

func (store *PostgresRunStore) Get(
	ctx context.Context, evidenceID string,
) (RunRecord, error) {
	if store == nil || store.pool == nil {
		return RunRecord{}, errors.New("rollout run store is unavailable")
	}
	return scanRunRecord(store.pool.QueryRow(ctx, rolloutRunSelect+
		" WHERE evidence_id=$1", evidenceID))
}

// rolloutRunColumns are the columns scanRunRecord reads, in its order.
const rolloutRunColumns = `evidence_id, source_instance,
	prior_evidence_id, policy, state,
	COALESCE(aggregate_regression_pct, 0)::float8,
	applied_instances, created_at, updated_at, COALESCE(halt_reason, ''),
	COALESCE(family, ''), COALESCE(action_class, ''), COALESCE(started_by, ''),
	targets`

const rolloutRunSelect = `SELECT ` + rolloutRunColumns + ` FROM sage.rollout_run`

type runRecordScanner interface {
	Scan(...any) error
}

func scanRunRecord(row runRecordScanner) (RunRecord, error) {
	var record RunRecord
	var policy, targets []byte
	err := row.Scan(
		&record.EvidenceID, &record.SourceInstance, &record.PriorEvidenceID,
		&policy, &record.State, &record.AggregateRegressionPct,
		&record.AppliedInstances, &record.CreatedAt, &record.UpdatedAt,
		&record.HaltReason, &record.Family, &record.Class, &record.StartedBy, &targets,
	)
	if err != nil {
		return RunRecord{}, err
	}
	if err := json.Unmarshal(policy, &record.Policy); err != nil {
		return RunRecord{}, fmt.Errorf("decode rollout policy: %w", err)
	}
	if err := json.Unmarshal(targets, &record.Targets); err != nil {
		return RunRecord{}, fmt.Errorf("decode rollout targets: %w", err)
	}
	return record, nil
}
