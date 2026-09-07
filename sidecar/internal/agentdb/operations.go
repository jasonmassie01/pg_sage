package agentdb

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) UpsertRecommendation(
	ctx context.Context,
	id string,
	req RecommendationCreate,
) (Recommendation, error) {
	if err := s.Ensure(ctx); err != nil {
		return Recommendation{}, err
	}
	if req.Kind == "" || req.Title == "" {
		return Recommendation{}, ErrInvalid
	}
	if req.RecommendationID == "" {
		req.RecommendationID = "rec_" + idFrom(id, req.Kind, req.QueryFingerprint)
	}
	if req.ActionRisk == "" {
		req.ActionRisk = "review"
	}
	var rec Recommendation
	err := scanRecommendation(s.pool.QueryRow(ctx, upsertRecommendationSQL,
		req.RecommendationID,
		id,
		req.Kind,
		req.Title,
		req.Detail,
		req.QueryFingerprint,
		req.ActionType,
		req.ActionRisk,
		req.Confidence,
		jsonBytes(req.AgentInstructions),
		jsonBytes(req.Payload),
	), &rec)
	return rec, err
}

func (s *Store) Recommendations(
	ctx context.Context,
	id string,
) ([]Recommendation, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ 
		SELECT recommendation_id, kind, title, detail, status,
			query_fingerprint, action_type, action_risk, confidence,
			agent_instructions, payload, feedback, created_at
		FROM sage.agent_db_recommendations
		WHERE deployment_id=$1
		ORDER BY created_at DESC`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recommendation{}
	for rows.Next() {
		var rec Recommendation
		if err := scanRecommendation(rows, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) Feedback(
	ctx context.Context,
	id string,
	recID string,
	req FeedbackRequest,
) error {
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	if req.Decision == "" {
		return ErrInvalid
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_recommendations
		SET status=$3, feedback=$4::jsonb, updated_at=now()
		WHERE deployment_id=$1 AND recommendation_id=$2`,
		id, recID, req.Decision, jsonBytes(map[string]any{
			"applied": req.Applied,
			"comment": req.Comment,
			"error":   req.Error,
			"result":  req.Result,
		}),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.audit(ctx, id, "recommendation_feedback", map[string]any{
		"recommendation_id": recID,
		"decision":          req.Decision,
	})
}

func (s *Store) AddCostSample(
	ctx context.Context,
	id string,
	req CostSampleRequest,
) error {
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	if req.At.IsZero() {
		req.At = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_db_cost_samples (
			deployment_id, sampled_at, cost_usd, metric, value, unit, detail
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		id, req.At, req.CostUSD, req.Metric, req.Value, req.Unit,
		jsonBytes(req.Detail),
	)
	if err != nil {
		return err
	}
	return s.applyBudgetStatus(ctx, id)
}

func (s *Store) Cost(ctx context.Context, id string) (CostSummary, error) {
	if err := s.Ensure(ctx); err != nil {
		return CostSummary{}, err
	}
	dep, err := s.Get(ctx, id)
	if err != nil {
		return CostSummary{}, err
	}
	var summary CostSummary
	err = s.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT COALESCE(sum(cost_usd), 0), count(*), max(sampled_at)
		FROM sage.agent_db_cost_samples
		WHERE deployment_id=$1`,
		id,
	).Scan(&summary.TotalUSD, &summary.SampleCount, &summary.LastSampleAt)
	if err != nil {
		return CostSummary{}, err
	}
	decision := BudgetStatus(dep, summary)
	summary.DeploymentID = id
	summary.BudgetUSD = dep.BudgetUSD
	summary.BudgetState = decision.State
	summary.BudgetAction = decision.Action
	return summary, nil
}

func (s *Store) RecordBackup(
	ctx context.Context,
	id string,
	req BackupRequest,
) (Backup, error) {
	if err := s.Ensure(ctx); err != nil {
		return Backup{}, err
	}
	if req.Status == "" {
		req.Status = "ready"
	}
	if req.BackupID == "" {
		req.BackupID = "backup_" + idFrom(id, req.Status, time.Now().String())
	}
	verifiedAt, restoreAt := backupTimes(req)
	var backup Backup
	err := scanBackup(s.pool.QueryRow(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_db_backups (
			backup_id, deployment_id, provider, status, archive_uri,
			verified_at, restore_verified_at, detail
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
		ON CONFLICT (backup_id) DO UPDATE
		SET provider=EXCLUDED.provider,
			status=EXCLUDED.status,
			archive_uri=EXCLUDED.archive_uri,
			verified_at=EXCLUDED.verified_at,
			restore_verified_at=EXCLUDED.restore_verified_at,
			detail=EXCLUDED.detail
		RETURNING backup_id, deployment_id, provider, status, archive_uri,
			verified_at, restore_verified_at, created_at, detail`,
		req.BackupID,
		id,
		req.Provider,
		req.Status,
		req.ArchiveURI,
		verifiedAt,
		restoreAt,
		jsonBytes(req.Detail),
	), &backup)
	if err != nil {
		return Backup{}, err
	}
	_ = s.audit(ctx, id, "backup_"+req.Status, map[string]any{
		"backup_id": req.BackupID,
		"provider":  req.Provider,
	})
	return backup, nil
}

func (s *Store) Backups(ctx context.Context, id string) ([]Backup, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ 
		SELECT backup_id, deployment_id, provider, status, archive_uri,
			verified_at, restore_verified_at, created_at, detail
		FROM sage.agent_db_backups
		WHERE deployment_id=$1
		ORDER BY created_at DESC`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Backup{}
	for rows.Next() {
		var backup Backup
		if err := scanBackup(rows, &backup); err != nil {
			return nil, err
		}
		out = append(out, backup)
	}
	return out, rows.Err()
}

func (s *Store) TuningHints(ctx context.Context, id string) ([]TuningHint, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ 
		SELECT hint_id, kind, title, detail, severity, payload
		FROM sage.agent_db_tuning_hints
		WHERE deployment_id=$1
		ORDER BY kind, title`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TuningHint{}
	for rows.Next() {
		var hint TuningHint
		if err := rows.Scan(
			&hint.HintID,
			&hint.Kind,
			&hint.Title,
			&hint.Detail,
			&hint.Severity,
			&hint.Payload,
		); err != nil {
			return nil, err
		}
		out = append(out, hint)
	}
	return out, rows.Err()
}

func (s *Store) audit(ctx context.Context, id, event string, detail map[string]any) error {
	_, err := s.pool.Exec(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_db_audit(deployment_id, event, detail)
		VALUES ($1, $2, $3::jsonb)`,
		id, event, jsonBytes(RedactProviderDetail(detail)),
	)
	return err
}

func (s *Store) applyBudgetStatus(ctx context.Context, id string) error {
	cost, err := s.Cost(ctx, id)
	if err != nil {
		return err
	}
	if cost.BudgetState != "hard_limit" {
		return nil
	}
	_, err = s.pool.Exec(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_deployments
		SET status='budget_exceeded', updated_at=now()
		WHERE deployment_id=$1 AND status='active'`,
		id,
	)
	return err
}

func (s *Store) seedTuningHints(
	ctx context.Context,
	id string,
	metadata map[string]any,
) error {
	for _, hint := range BuildTuningHints(tuningContext(metadata)) {
		if _, err := s.pool.Exec(ctx, `/* pg_sage */ 
			INSERT INTO sage.agent_db_tuning_hints (
				hint_id, deployment_id, kind, title, detail, severity, payload
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
			ON CONFLICT (hint_id, deployment_id) DO UPDATE
			SET title=EXCLUDED.title,
				detail=EXCLUDED.detail,
				severity=EXCLUDED.severity,
				payload=EXCLUDED.payload`,
			hint.HintID,
			id,
			hint.Kind,
			hint.Title,
			hint.Detail,
			hint.Severity,
			jsonBytes(hint.Payload),
		); err != nil {
			return err
		}
	}
	return nil
}

func tuningContext(metadata map[string]any) TuningContext {
	return TuningContext{
		WorkloadTypes: stringsFromAny(metadata["workload_types"]),
		Extensions:    stringsFromAny(metadata["extensions"]),
	}
}

func stringsFromAny(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	default:
		return nil
	}
}

func backupTimes(req BackupRequest) (*time.Time, *time.Time) {
	now := time.Now().UTC()
	verifiedAt := optionalTime(req.VerifiedAt)
	restoreAt := optionalTime(req.RestoreVerifiedAt)
	if req.Status == "verified" && verifiedAt == nil {
		verifiedAt = &now
	}
	if req.Status == "restore_verified" {
		if verifiedAt == nil {
			verifiedAt = &now
		}
		if restoreAt == nil {
			restoreAt = &now
		}
	}
	return verifiedAt, restoreAt
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func scanRecommendation(row scanner, rec *Recommendation) error {
	return row.Scan(
		&rec.ID,
		&rec.Kind,
		&rec.Title,
		&rec.Detail,
		&rec.Status,
		&rec.QueryFingerprint,
		&rec.ActionType,
		&rec.ActionRisk,
		&rec.Confidence,
		&rec.AgentInstructions,
		&rec.Payload,
		&rec.Feedback,
		&rec.CreatedAt,
	)
}

func scanBackup(row scanner, backup *Backup) error {
	err := row.Scan(
		&backup.BackupID,
		&backup.DeploymentID,
		&backup.Provider,
		&backup.Status,
		&backup.ArchiveURI,
		&backup.VerifiedAt,
		&backup.RestoreVerifiedAt,
		&backup.CreatedAt,
		&backup.Detail,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const upsertRecommendationSQL = `/* pg_sage */
	INSERT INTO sage.agent_db_recommendations (
		recommendation_id, deployment_id, kind, title, detail,
		query_fingerprint, action_type, action_risk, confidence,
		agent_instructions, payload
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb)
	ON CONFLICT (recommendation_id) DO UPDATE
	SET kind=EXCLUDED.kind,
		title=EXCLUDED.title,
		detail=EXCLUDED.detail,
		query_fingerprint=EXCLUDED.query_fingerprint,
		action_type=EXCLUDED.action_type,
		action_risk=EXCLUDED.action_risk,
		confidence=EXCLUDED.confidence,
		agent_instructions=EXCLUDED.agent_instructions,
		payload=EXCLUDED.payload,
		updated_at=now()
	RETURNING recommendation_id, kind, title, detail, status,
		query_fingerprint, action_type, action_risk, confidence,
		agent_instructions, payload, feedback, created_at`
