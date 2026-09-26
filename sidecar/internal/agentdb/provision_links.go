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
	profile.ProfileID = "blueprint_" + idFrom(blueprint.BlueprintID, blueprint.TemplateID)
	profile = normalizedProvisionProfile(profile)
	plan, err := BuildProvisionPlan(reg, profile)
	if err != nil {
		return Deployment{}, err
	}
	reg.ProvisioningStatus = "planned"
	reg.SizeProfileID = profile.ProfileID
	reg.ProvisioningPlan = planMap(plan)
	reg.ProvisioningPlan["source"] = "blueprint"
	reg.Metadata = mergeMap(reg.Metadata, map[string]any{
		"blueprint_id":          blueprint.BlueprintID,
		"terraform_template_id": blueprint.TemplateID,
		"provider_params":       cloneAnyMap(profile.ProviderParams),
		"size_profile_id":       profile.ProfileID,
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
		ProfileID:         "terraform_" + idFrom(template.TemplateID),
		Provider:          req.Provider,
		ProvisioningLevel: LevelInstance,
		ProviderParams:    req.ProviderParams,
	}
	profile = normalizedProvisionProfile(profile)
	plan, err := BuildProvisionPlan(reg, profile)
	if err != nil {
		return Deployment{}, err
	}
	reg.ProvisioningStatus = "planned"
	reg.SizeProfileID = profile.ProfileID
	reg.Metadata["provider_params"] = cloneAnyMap(profile.ProviderParams)
	reg.Metadata["size_profile_id"] = profile.ProfileID
	reg.ProvisioningPlan = planMap(plan)
	reg.ProvisioningPlan["source"] = "terraform_template"
	reg.ProvisioningPlan["terraform_template_id"] = template.TemplateID
	// SURF-05: the template is a reviewed reference, not an executed plan.
	// The runner provisions from provider_params; say so and bind the
	// approval to the exact template content that was reviewed.
	reg.ProvisioningPlan["template_semantics"] = "review_only"
	reg.ProvisioningPlan["template_semantics_note"] = "template content is stored " +
		"for review only; the live runner provisions from provider_params"
	reg.Metadata["terraform_template_sha256"] = template.ContentSHA256
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
	deploymentID := firstNonEmpty(req.DeploymentID, "dep_"+idFrom(id))
	if err := s.consumeApprovedRequest(ctx, id, deploymentID); err != nil {
		return Deployment{}, err
	}
	reg := RegisterRequest{
		DeploymentID: deploymentID,
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
	// The approved spec wins; overrides only fill what the spec leaves open
	// (G8-B10). Settings the runners enforce are carried through (G8-B20).
	setApproved(params, "region", spec.Region)
	setApproved(params, "db_instance_class", spec.InstanceClass)
	setApproved(params, "tier", spec.InstanceClass)
	setApproved(params, "database_version", spec.DatabaseVersion)
	setApproved(params, "backup_retention_days", spec.BackupRetentionDays)
	setApproved(params, "allocated_storage", spec.StorageGB)
	setApproved(params, "storage_size", spec.StorageGB)
	setApproved(params, "mode", spec.LakebaseMode)
	if spec.MultiAZ {
		params["multi_az"] = true
	}
	if spec.PrivateNetwork {
		params["private_network"] = true
	}
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

func setApproved(values map[string]any, key string, value any) {
	if emptyAny(value) {
		return
	}
	values[key] = value
}

// consumeApprovedRequest makes an approval single-use: it binds the
// request to one deployment id; replays with the same id stay idempotent.
func (s *Store) consumeApprovedRequest(ctx context.Context, id, deploymentID string) error {
	tag, err := s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_requests
		SET consumed_deployment_id=$2, updated_at=now()
		WHERE request_id=$1 AND consumed_deployment_id IN ('', $2)`, id, deploymentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
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
