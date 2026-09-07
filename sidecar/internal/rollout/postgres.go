package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
	_, err = store.pool.Exec(ctx, `INSERT INTO sage.rollout_run
		(evidence_id, source_instance, prior_evidence_id, policy, state,
		 aggregate_regression_pct, applied_instances, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5,
		 NULLIF($6::double precision, 0)::numeric, $7, $8, $9)`,
		record.EvidenceID, record.SourceInstance, record.PriorEvidenceID, policy,
		record.State, record.AggregateRegressionPct, record.AppliedInstances,
		record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert rollout run: %w", err)
	}
	return nil
}

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
		updated_at=$8 WHERE evidence_id=$1`,
		record.EvidenceID, record.SourceInstance, record.PriorEvidenceID, policy,
		record.State, record.AggregateRegressionPct, record.AppliedInstances,
		record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update rollout run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("rollout run %q not found", record.EvidenceID)
	}
	return nil
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

func (store *PostgresRunStore) Latest(
	ctx context.Context,
) (RunRecord, bool, error) {
	if store == nil || store.pool == nil {
		return RunRecord{}, false, errors.New("rollout run store is unavailable")
	}
	record, err := scanRunRecord(store.pool.QueryRow(ctx, rolloutRunSelect+
		" ORDER BY updated_at DESC, id DESC LIMIT 1"))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRecord{}, false, nil
	}
	return record, err == nil, err
}

const rolloutRunSelect = `SELECT evidence_id, source_instance,
	prior_evidence_id, policy, state,
	COALESCE(aggregate_regression_pct, 0)::float8,
	applied_instances, created_at, updated_at FROM sage.rollout_run`

type runRecordScanner interface {
	Scan(...any) error
}

func scanRunRecord(row runRecordScanner) (RunRecord, error) {
	var record RunRecord
	var policy []byte
	err := row.Scan(
		&record.EvidenceID, &record.SourceInstance, &record.PriorEvidenceID,
		&policy, &record.State, &record.AggregateRegressionPct,
		&record.AppliedInstances, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return RunRecord{}, err
	}
	if err := json.Unmarshal(policy, &record.Policy); err != nil {
		return RunRecord{}, fmt.Errorf("decode rollout policy: %w", err)
	}
	return record, nil
}

var _ RuntimeRunStore = (*PostgresRunStore)(nil)
