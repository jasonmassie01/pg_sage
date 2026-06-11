package agentdb

import (
	"context"
)

func (s *Store) GetBlueprint(ctx context.Context, id string) (Blueprint, error) {
	if err := s.Ensure(ctx); err != nil {
		return Blueprint{}, err
	}
	var blueprint Blueprint
	err := scanBlueprint(s.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT blueprint_id, name, status, intent, provider, template_id,
			blueprint_json, policy_findings, llm_used, raw_response, created_by,
			approved_by, created_at, updated_at
		FROM sage.agent_db_blueprints
		WHERE blueprint_id=$1`, id), &blueprint)
	return blueprint, err
}

func (s *Store) ApproveBlueprint(
	ctx context.Context,
	id string,
	approvedBy string,
) (Blueprint, error) {
	if err := s.Ensure(ctx); err != nil {
		return Blueprint{}, err
	}
	blueprint, err := s.GetBlueprint(ctx, id)
	if err != nil {
		return Blueprint{}, err
	}
	if len(blueprint.PolicyFindings) > 0 || blueprint.Status == "rejected" {
		return Blueprint{}, ErrInvalid
	}
	var out Blueprint
	err = scanBlueprint(s.pool.QueryRow(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_blueprints
		SET status='approved', approved_by=$2, updated_at=now()
		WHERE blueprint_id=$1
		RETURNING blueprint_id, name, status, intent, provider, template_id,
			blueprint_json, policy_findings, llm_used, raw_response, created_by,
			approved_by, created_at, updated_at`, id, approvedBy), &out)
	return out, err
}

func (s *Store) ProvisionFromBlueprint(
	ctx context.Context,
	id string,
	req BlueprintProvisionRequest,
) (Deployment, error) {
	blueprint, err := s.GetBlueprint(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if blueprint.Status != "approved" {
		return Deployment{}, ErrInvalid
	}
	reg := registerFromBlueprint(blueprint, req)
	profile := profileFromBlueprint(blueprint.Spec, req.ProviderParams)
	plan, err := BuildProvisionPlan(reg, profile)
	if err != nil {
		return Deployment{}, err
	}
	reg.ProvisioningStatus = "planned"
	reg.ProvisioningPlan = planMap(plan)
	reg.ProvisioningPlan["source"] = "blueprint"
	reg.Metadata = mergeMap(reg.Metadata, map[string]any{
		"blueprint_id":          blueprint.BlueprintID,
		"terraform_template_id": blueprint.TemplateID,
	})
	return s.Register(ctx, reg)
}

func (s *Store) GetTerraformTemplate(
	ctx context.Context,
	id string,
) (TerraformTemplate, error) {
	if err := s.Ensure(ctx); err != nil {
		return TerraformTemplate{}, err
	}
	var template TerraformTemplate
	err := scanTerraformTemplate(s.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT template_id, name, status, source_kind, content_sha256,
			files_json, manifest_json, policy_findings, created_by, approved_by,
			created_at, updated_at
		FROM sage.agent_db_terraform_templates
		WHERE template_id=$1`, id), &template)
	return template, err
}

func (s *Store) ProvisionFromTerraformTemplate(
	ctx context.Context,
	id string,
	req TemplateProvisionRequest,
) (Deployment, error) {
	template, err := s.GetTerraformTemplate(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if template.Status != "approved" {
		return Deployment{}, ErrInvalid
	}
	reg := RegisterRequest{
		DeploymentID:      firstNonEmpty(req.DeploymentID, "dep_"+idFrom(id, req.AgentID)),
		TenantID:          req.TenantID,
		AgentID:           req.AgentID,
		RunID:             req.RunID,
		DatabaseName:      req.DatabaseName,
		Provider:          req.Provider,
		ProvisioningLevel: firstNonEmpty(req.ProvisioningLevel, LevelInstance),
		LeaseSeconds:      req.LeaseSeconds,
		BudgetUSD:         req.BudgetUSD,
		BackupRequired:    true,
		Metadata: mergeMap(req.Metadata, map[string]any{
			"provider_params":       req.ProviderParams,
			"terraform_template_id": template.TemplateID,
		}),
	}
	if reg.TenantID == "" || reg.AgentID == "" {
		return Deployment{}, ErrInvalid
	}
	profile := SizeProfile{
		Provider:          req.Provider,
		ProvisioningLevel: LevelInstance,
		ProviderParams:    req.ProviderParams,
	}
	plan, err := BuildProvisionPlan(reg, profile)
	if err != nil {
		return Deployment{}, err
	}
	reg.ProvisioningStatus = "planned"
	reg.ProvisioningPlan = planMap(plan)
	reg.ProvisioningPlan["source"] = "terraform_template"
	reg.ProvisioningPlan["terraform_template_id"] = template.TemplateID
	return s.Register(ctx, reg)
}

func (s *Store) ProvisionApprovedRequest(
	ctx context.Context,
	id string,
	req RequestProvisionRequest,
) (Deployment, error) {
	agentReq, err := s.GetRequest(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if agentReq.Status != "approved" || agentReq.PolicyDecision != "allow" {
		return Deployment{}, ErrInvalid
	}
	reg := RegisterRequest{
		DeploymentID: firstNonEmpty(req.DeploymentID, "dep_"+idFrom(id)),
		TenantID:     agentReq.TenantID,
		AgentID:      agentReq.AgentID,
		RunID:        agentReq.RunID,
		DatabaseName: agentReq.DatabaseName,
		Provider:     agentReq.Provider,
		ProvisioningLevel: firstNonEmpty(
			agentReq.IsolationType, LevelSchema,
		),
		LeaseSeconds:   req.LeaseSeconds,
		BudgetUSD:      agentReq.BudgetUSD,
		BackupRequired: agentReq.BackupRequired,
		Metadata: mergeMap(req.Metadata, map[string]any{
			"provider_params": req.ProviderParams,
			"request_id":      agentReq.RequestID,
			"purpose":         agentReq.Purpose,
		}),
	}
	return s.Provision(ctx, reg)
}

func registerFromBlueprint(
	blueprint Blueprint,
	req BlueprintProvisionRequest,
) RegisterRequest {
	spec := NormalizeBlueprintSpec(blueprint.Spec, blueprint.Intent)
	metadata := mergeMap(req.Metadata, map[string]any{
		"extensions":      stringsAny(spec.Extensions),
		"lakebase_mode":   spec.LakebaseMode,
		"provider_params": req.ProviderParams,
		"workload_source": "blueprint",
	})
	return RegisterRequest{
		DeploymentID: firstNonEmpty(
			req.DeploymentID, "dep_"+idFrom(blueprint.BlueprintID, req.AgentID),
		),
		TenantID:          req.TenantID,
		AgentID:           req.AgentID,
		RunID:             req.RunID,
		DatabaseName:      firstNonEmpty(req.DatabaseName, blueprint.Name),
		Provider:          spec.Provider,
		ProvisioningLevel: spec.ProvisioningLevel,
		LeaseSeconds:      req.LeaseSeconds,
		BudgetUSD:         firstNonZero(req.BudgetUSD, spec.BudgetUSD),
		BackupRequired:    true,
		Metadata:          metadata,
	}
}

func profileFromBlueprint(
	spec BlueprintSpec,
	overrides map[string]any,
) SizeProfile {
	params := map[string]any{}
	for key, value := range overrides {
		params[key] = value
	}
	setIfMissing(params, "region", spec.Region)
	setIfMissing(params, "db_instance_class", spec.InstanceClass)
	setIfMissing(params, "tier", spec.InstanceClass)
	setIfMissing(params, "database_version", spec.DatabaseVersion)
	setIfMissing(params, "backup_retention_days", spec.BackupRetentionDays)
	setIfMissing(params, "allocated_storage", spec.StorageGB)
	setIfMissing(params, "storage_size", spec.StorageGB)
	setIfMissing(params, "mode", spec.LakebaseMode)
	return SizeProfile{
		Provider:          spec.Provider,
		ProvisioningLevel: spec.ProvisioningLevel,
		StorageGB:         float64(spec.StorageGB),
		MonthlyBudgetUSD:  spec.BudgetUSD,
		ProviderParams:    params,
	}
}

func mergeMap(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		if value != nil && value != "" {
			out[key] = value
		}
	}
	return out
}

func setIfMissing(values map[string]any, key string, value any) {
	if _, ok := values[key]; ok || emptyAny(value) {
		return
	}
	values[key] = value
}

func emptyAny(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case int:
		return v == 0
	case float64:
		return v == 0
	default:
		return false
	}
}

func firstNonZero(values ...float64) float64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func stringsAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
