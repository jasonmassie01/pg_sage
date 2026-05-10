package agentdb

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateTerraformTemplate(
	ctx context.Context,
	req TerraformTemplateRequest,
) (TerraformTemplate, error) {
	if err := s.Ensure(ctx); err != nil {
		return TerraformTemplate{}, err
	}
	template := ValidateTerraformTemplate(req)
	err := scanTerraformTemplate(s.pool.QueryRow(ctx, `
		INSERT INTO sage.agent_db_terraform_templates (
			template_id, name, status, source_kind, content_sha256,
			files_json, manifest_json, policy_findings, created_by
		)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8::jsonb, $9)
		ON CONFLICT (template_id) DO UPDATE
		SET name=EXCLUDED.name,
			status=EXCLUDED.status,
			source_kind=EXCLUDED.source_kind,
			content_sha256=EXCLUDED.content_sha256,
			files_json=EXCLUDED.files_json,
			manifest_json=EXCLUDED.manifest_json,
			policy_findings=EXCLUDED.policy_findings,
			updated_at=now()
		RETURNING template_id, name, status, source_kind, content_sha256,
			files_json, manifest_json, policy_findings, created_by, approved_by,
			created_at, updated_at`,
		template.TemplateID,
		template.Name,
		template.Status,
		template.SourceKind,
		template.ContentSHA256,
		jsonAny(template.Files),
		jsonBytes(template.Manifest),
		jsonAny(template.PolicyFindings),
		template.CreatedBy,
	), &template)
	return template, err
}

func (s *Store) TerraformTemplates(ctx context.Context) ([]TerraformTemplate, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT template_id, name, status, source_kind, content_sha256,
			files_json, manifest_json, policy_findings, created_by, approved_by,
			created_at, updated_at
		FROM sage.agent_db_terraform_templates
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TerraformTemplate{}
	for rows.Next() {
		var template TerraformTemplate
		if err := scanTerraformTemplate(rows, &template); err != nil {
			return nil, err
		}
		out = append(out, template)
	}
	return out, rows.Err()
}

func (s *Store) ApproveTerraformTemplate(
	ctx context.Context,
	id string,
	approvedBy string,
) (TerraformTemplate, error) {
	if err := s.Ensure(ctx); err != nil {
		return TerraformTemplate{}, err
	}
	var findings []string
	if err := s.pool.QueryRow(ctx, `
		SELECT policy_findings FROM sage.agent_db_terraform_templates
		WHERE template_id=$1`, id).Scan(&findings); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TerraformTemplate{}, ErrNotFound
		}
		return TerraformTemplate{}, err
	}
	if len(findings) > 0 {
		return TerraformTemplate{}, ErrInvalid
	}
	var template TerraformTemplate
	err := scanTerraformTemplate(s.pool.QueryRow(ctx, `
		UPDATE sage.agent_db_terraform_templates
		SET status='approved', approved_by=$2, updated_at=now()
		WHERE template_id=$1
		RETURNING template_id, name, status, source_kind, content_sha256,
			files_json, manifest_json, policy_findings, created_by, approved_by,
			created_at, updated_at`, id, approvedBy), &template)
	return template, err
}

func scanTerraformTemplate(row scanner, template *TerraformTemplate) error {
	var filesJSON []byte
	var findingsJSON []byte
	err := row.Scan(
		&template.TemplateID,
		&template.Name,
		&template.Status,
		&template.SourceKind,
		&template.ContentSHA256,
		&filesJSON,
		&template.Manifest,
		&findingsJSON,
		&template.CreatedBy,
		&template.ApprovedBy,
		&template.CreatedAt,
		&template.UpdatedAt,
	)
	if err != nil {
		return err
	}
	_ = json.Unmarshal(filesJSON, &template.Files)
	_ = json.Unmarshal(findingsJSON, &template.PolicyFindings)
	return nil
}
