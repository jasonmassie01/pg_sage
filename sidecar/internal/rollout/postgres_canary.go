package rollout

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// InstanceRecord is one database's part in a fleet canary.
type InstanceRecord struct {
	InstanceID    string    `json:"database"`
	Ordinal       int       `json:"ordinal"`
	Status        string    `json:"status"`
	EvidenceID    string    `json:"evidence_id,omitempty"`
	ActionLogID   int64     `json:"action_log_id,omitempty"`
	RegressionPct float64   `json:"regression_pct"`
	Detail        string    `json:"detail,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SaveInstances upserts a run's instance results.
func (store *PostgresRunStore) SaveInstances(ctx context.Context, evidenceID string,
	records []InstanceRecord) error {
	if store == nil || store.pool == nil {
		return errors.New("rollout run store is unavailable")
	}
	for _, r := range records {
		var actionLogID any
		if r.ActionLogID > 0 {
			actionLogID = r.ActionLogID
		}
		_, err := store.pool.Exec(ctx, `INSERT INTO sage.rollout_instance
			(run_evidence_id, instance_id, ordinal, status, evidence_id, action_log_id,
			 regression_pct, detail, updated_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), $9)
			ON CONFLICT (run_evidence_id, instance_id) DO UPDATE SET
			  ordinal = EXCLUDED.ordinal, status = EXCLUDED.status,
			  evidence_id = EXCLUDED.evidence_id, action_log_id = EXCLUDED.action_log_id,
			  regression_pct = EXCLUDED.regression_pct, detail = EXCLUDED.detail,
			  updated_at = EXCLUDED.updated_at`,
			evidenceID, r.InstanceID, r.Ordinal, r.Status, r.EvidenceID, actionLogID,
			r.RegressionPct, truncateDetail(r.Detail), r.UpdatedAt)
		if err != nil {
			return fmt.Errorf("save rollout instance %s: %w", r.InstanceID, err)
		}
	}
	return nil
}

func truncateDetail(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// Instances lists a run's instances in rollout order.
func (store *PostgresRunStore) Instances(ctx context.Context, evidenceID string) (
	[]InstanceRecord, error) {
	if store == nil || store.pool == nil {
		return nil, errors.New("rollout run store is unavailable")
	}
	rows, err := store.pool.Query(ctx, `SELECT instance_id, ordinal, status,
		COALESCE(evidence_id, ''), COALESCE(action_log_id, 0),
		COALESCE(regression_pct, 0), COALESCE(detail, ''), updated_at
		FROM sage.rollout_instance WHERE run_evidence_id = $1 ORDER BY ordinal`, evidenceID)
	if err != nil {
		return nil, fmt.Errorf("list rollout instances: %w", err)
	}
	defer rows.Close()
	out := []InstanceRecord{}
	for rows.Next() {
		var r InstanceRecord
		if err := rows.Scan(&r.InstanceID, &r.Ordinal, &r.Status, &r.EvidenceID,
			&r.ActionLogID, &r.RegressionPct, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan rollout instance: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// List lists the newest runs.
func (store *PostgresRunStore) List(ctx context.Context, limit int) ([]RunRecord, error) {
	if store == nil || store.pool == nil {
		return nil, errors.New("rollout run store is unavailable")
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := store.pool.Query(ctx, rolloutRunSelect+
		" ORDER BY updated_at DESC, id DESC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("list rollout runs: %w", err)
	}
	defer rows.Close()
	out := []RunRecord{}
	for rows.Next() {
		record, err := scanRunRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rollout run: %w", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
