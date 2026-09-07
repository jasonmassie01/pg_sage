package agentdb

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateDeployRequest(
	ctx context.Context,
	id string,
	req DeployRequestCreate,
) (DeployRequest, error) {
	if err := s.Ensure(ctx); err != nil {
		return DeployRequest{}, err
	}
	dep, err := s.Get(ctx, id)
	if err != nil {
		return DeployRequest{}, err
	}
	normalizeDeployRequest(&req)
	if err := validateDeployRequestCreate(req); err != nil {
		return DeployRequest{}, err
	}
	if req.DeployRequestID == "" {
		req.DeployRequestID = "dr_" + idFrom(id, req.Title, req.MigrationSQL)
	}
	gates := deployGateResults(req)
	var out DeployRequest
	err = scanDeployRequest(s.pool.QueryRow(ctx, upsertDeployRequestSQL,
		req.DeployRequestID,
		dep.DeploymentID,
		dep.TenantID,
		dep.AgentID,
		dep.RunID,
		req.TargetDatabaseName,
		req.TargetSchemaName,
		req.Title,
		req.Reason,
		req.Status,
		req.RiskTier,
		req.MigrationSQL,
		req.VerificationSQL,
		req.RollbackSQL,
		req.ForwardFixNotes,
		jsonBytes(gates),
		req.CreatedBy,
	), &out)
	if err != nil {
		return DeployRequest{}, err
	}
	_ = s.audit(ctx, id, "deploy_request_created", map[string]any{
		"deploy_request_id": out.DeployRequestID,
		"status":            out.Status,
		"risk_tier":         out.RiskTier,
	})
	if out.Status == "review_requested" {
		_ = s.audit(ctx, id, "deploy_request_review_requested", map[string]any{
			"deploy_request_id": out.DeployRequestID,
		})
	}
	return out, nil
}

func (s *Store) DeployRequests(ctx context.Context, id string) ([]DeployRequest, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, selectDeployRequestsSQL+`
		WHERE deployment_id=$1
		ORDER BY created_at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployRequest{}
	for rows.Next() {
		var req DeployRequest
		if err := scanDeployRequest(rows, &req); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

func (s *Store) GetDeployRequest(
	ctx context.Context,
	id string,
	requestID string,
) (DeployRequest, error) {
	if err := s.Ensure(ctx); err != nil {
		return DeployRequest{}, err
	}
	var req DeployRequest
	err := scanDeployRequest(s.pool.QueryRow(ctx, selectDeployRequestsSQL+`
		WHERE deployment_id=$1 AND deploy_request_id=$2`,
		id, requestID), &req)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployRequest{}, ErrNotFound
	}
	return req, err
}

func (s *Store) RequestDeployReview(
	ctx context.Context,
	id string,
	requestID string,
) (DeployRequest, error) {
	req, err := s.GetDeployRequest(ctx, id, requestID)
	if err != nil {
		return DeployRequest{}, err
	}
	if req.Status != "draft" {
		return DeployRequest{}, ErrConflict
	}
	if strings.TrimSpace(req.MigrationSQL) == "" ||
		strings.TrimSpace(req.VerificationSQL) == "" {
		return DeployRequest{}, ErrInvalid
	}
	updated, err := s.updateDeployRequestStatus(ctx, id, requestID,
		"review_requested", "", "", "deploy_request_review_requested")
	if err != nil {
		return DeployRequest{}, err
	}
	return updated, nil
}

func (s *Store) ReviewDeployRequest(
	ctx context.Context,
	id string,
	requestID string,
	review DeployRequestReview,
) (DeployRequest, error) {
	if strings.TrimSpace(review.ReviewReason) == "" {
		return DeployRequest{}, ErrInvalid
	}
	status, event, err := deployReviewDecision(review.Decision)
	if err != nil {
		return DeployRequest{}, err
	}
	req, err := s.GetDeployRequest(ctx, id, requestID)
	if err != nil {
		return DeployRequest{}, err
	}
	if req.Status != "review_requested" {
		return DeployRequest{}, ErrConflict
	}
	return s.updateDeployRequestStatus(ctx, id, requestID, status,
		review.ReviewedBy, review.ReviewReason, event)
}

func (s *Store) updateDeployRequestStatus(
	ctx context.Context,
	id string,
	requestID string,
	status string,
	reviewedBy string,
	reason string,
	event string,
) (DeployRequest, error) {
	tag, err := s.pool.Exec(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_deploy_requests
		SET status=$3,
			reviewed_by=CASE WHEN $4='' THEN reviewed_by ELSE $4 END,
			review_reason=CASE WHEN $5='' THEN review_reason ELSE $5 END,
			reviewed_at=CASE WHEN $4='' THEN reviewed_at ELSE now() END,
			updated_at=now()
		WHERE deployment_id=$1 AND deploy_request_id=$2`,
		id, requestID, status, reviewedBy, reason)
	if err != nil {
		return DeployRequest{}, err
	}
	if tag.RowsAffected() == 0 {
		return DeployRequest{}, ErrNotFound
	}
	_ = s.audit(ctx, id, event, map[string]any{
		"deploy_request_id": requestID,
		"reviewed_by":       reviewedBy,
		"reason":            reason,
	})
	return s.GetDeployRequest(ctx, id, requestID)
}

func normalizeDeployRequest(req *DeployRequestCreate) {
	req.Title = strings.TrimSpace(req.Title)
	req.Status = strings.TrimSpace(req.Status)
	if req.Status == "" {
		req.Status = "draft"
	}
	req.RiskTier = strings.TrimSpace(req.RiskTier)
	if req.RiskTier == "" {
		req.RiskTier = "moderate"
	}
}

func validateDeployRequestCreate(req DeployRequestCreate) error {
	if req.Title == "" {
		return ErrInvalid
	}
	switch req.Status {
	case "draft", "review_requested":
	default:
		return ErrInvalid
	}
	if req.Status == "review_requested" &&
		(strings.TrimSpace(req.MigrationSQL) == "" ||
			strings.TrimSpace(req.VerificationSQL) == "") {
		return ErrInvalid
	}
	return nil
}

func deployGateResults(req DeployRequestCreate) map[string]any {
	gates := map[string]any{}
	for key, value := range req.GateResults {
		gates[key] = value
	}
	gates["review_only"] = true
	gates["has_migration_sql"] = strings.TrimSpace(req.MigrationSQL) != ""
	gates["has_verification_sql"] = strings.TrimSpace(req.VerificationSQL) != ""
	gates["has_rollback_or_forward_fix"] = strings.TrimSpace(req.RollbackSQL) != "" ||
		strings.TrimSpace(req.ForwardFixNotes) != ""
	gates["migration_statement_count"] = statementCount(req.MigrationSQL)
	return gates
}

func statementCount(sql string) int {
	count := 0
	for _, part := range strings.Split(sql, ";") {
		if strings.TrimSpace(part) != "" {
			count++
		}
	}
	return count
}

func deployReviewDecision(decision string) (string, string, error) {
	switch decision {
	case "approved":
		return "approved", "deploy_request_approved", nil
	case "denied":
		return "denied", "deploy_request_denied", nil
	default:
		return "", "", ErrInvalid
	}
}

func scanDeployRequest(row scanner, req *DeployRequest) error {
	return row.Scan(
		&req.DeployRequestID,
		&req.DeploymentID,
		&req.TenantID,
		&req.AgentID,
		&req.RunID,
		&req.TargetDatabaseName,
		&req.TargetSchemaName,
		&req.Title,
		&req.Reason,
		&req.Status,
		&req.RiskTier,
		&req.MigrationSQL,
		&req.VerificationSQL,
		&req.RollbackSQL,
		&req.ForwardFixNotes,
		&req.GateResults,
		&req.CreatedBy,
		&req.ReviewedBy,
		&req.ReviewReason,
		&req.CreatedAt,
		&req.UpdatedAt,
		&req.ReviewedAt,
	)
}

const selectDeployRequestsSQL = `/* pg_sage */
	SELECT deploy_request_id, deployment_id, tenant_id, agent_id, run_id,
		target_database_name, target_schema_name, title, reason, status,
		risk_tier, migration_sql, verification_sql, rollback_sql,
		forward_fix_notes, gate_results, created_by, reviewed_by,
		review_reason, created_at, updated_at, reviewed_at
	FROM sage.agent_db_deploy_requests`

const upsertDeployRequestSQL = `/* pg_sage */
	INSERT INTO sage.agent_db_deploy_requests (
		deploy_request_id, deployment_id, tenant_id, agent_id, run_id,
		target_database_name, target_schema_name, title, reason, status,
		risk_tier, migration_sql, verification_sql, rollback_sql,
		forward_fix_notes, gate_results, created_by
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		$12, $13, $14, $15, $16::jsonb, $17)
	ON CONFLICT (deploy_request_id) DO UPDATE
	SET target_database_name=EXCLUDED.target_database_name,
		target_schema_name=EXCLUDED.target_schema_name,
		title=EXCLUDED.title,
		reason=EXCLUDED.reason,
		status=EXCLUDED.status,
		risk_tier=EXCLUDED.risk_tier,
		migration_sql=EXCLUDED.migration_sql,
		verification_sql=EXCLUDED.verification_sql,
		rollback_sql=EXCLUDED.rollback_sql,
		forward_fix_notes=EXCLUDED.forward_fix_notes,
		gate_results=EXCLUDED.gate_results,
		created_by=EXCLUDED.created_by,
		updated_at=now()
	RETURNING deploy_request_id, deployment_id, tenant_id, agent_id, run_id,
		target_database_name, target_schema_name, title, reason, status,
		risk_tier, migration_sql, verification_sql, rollback_sql,
		forward_fix_notes, gate_results, created_by, reviewed_by,
		review_reason, created_at, updated_at, reviewed_at`
