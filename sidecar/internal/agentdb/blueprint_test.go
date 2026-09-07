package agentdb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBlueprintFallbackParsesIntent(t *testing.T) {
	gen := NewHeuristicBlueprintGenerator()
	out, err := gen.GenerateBlueprint(context.Background(), BlueprintDraftRequest{
		BlueprintID: "bp_support_agent",
		Name:        "Support agent database",
		Provider:    ProviderAWSRDS,
		Intent: strings.Join([]string{
			"Create a private AWS RDS Postgres database in us-east-1.",
			"It needs Multi-AZ HA, PITR, 7-day backups, pgvector and PostGIS.",
			"Use 100 GB storage, a db.t4g.medium instance, and keep it under $400/month.",
		}, " "),
	})
	if err != nil {
		t.Fatalf("GenerateBlueprint: %v", err)
	}
	if out.LLMUsed {
		t.Fatal("heuristic generator should not mark LLMUsed")
	}
	if out.Spec.Provider != ProviderAWSRDS {
		t.Fatalf("provider = %q", out.Spec.Provider)
	}
	if out.Spec.Region != "us-east-1" {
		t.Fatalf("region = %q", out.Spec.Region)
	}
	if !out.Spec.MultiAZ || !out.Spec.PITR || !out.Spec.PrivateNetwork {
		t.Fatalf("ha/pitr/private flags = %#v", out.Spec)
	}
	if out.Spec.PublicIP {
		t.Fatalf("public ip should be false: %#v", out.Spec)
	}
	if out.Spec.StorageGB != 100 || out.Spec.BackupRetentionDays != 7 {
		t.Fatalf("storage/backups = %d/%d", out.Spec.StorageGB, out.Spec.BackupRetentionDays)
	}
	if !containsString(out.Spec.Extensions, "pgvector") ||
		!containsString(out.Spec.Extensions, "postgis") {
		t.Fatalf("extensions = %#v", out.Spec.Extensions)
	}
	if out.Spec.BudgetUSD != 400 {
		t.Fatalf("budget = %.2f", out.Spec.BudgetUSD)
	}
}

func TestBlueprintPolicyFindingsRejectUnsafePublicIP(t *testing.T) {
	spec := BlueprintSpec{
		Provider:            ProviderGCPCloudSQL,
		ProvisioningLevel:   LevelInstance,
		Region:              "us-central1",
		PublicIP:            true,
		BackupRetentionDays: 0,
	}
	findings := BlueprintPolicyFindings(spec, BlueprintPolicy{
		AllowPublicIP:              false,
		MinimumBackupRetentionDays: 1,
		RequirePrivateNetworking:   true,
	})
	if len(findings) < 3 {
		t.Fatalf("findings = %#v, want public ip, backup, and private network findings", findings)
	}
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"public ip", "backup", "private network"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("findings %q missing %q", joined, want)
		}
	}
}

func TestRenderTerraformFromBlueprintAWSRDS(t *testing.T) {
	files, err := RenderTerraformFromBlueprint(BlueprintSpec{
		Provider:            ProviderAWSRDS,
		ProvisioningLevel:   LevelInstance,
		Region:              "us-east-1",
		InstanceClass:       "db.t4g.medium",
		StorageGB:           100,
		BackupRetentionDays: 7,
		MultiAZ:             true,
		PITR:                true,
		PrivateNetwork:      true,
		Extensions:          []string{"pgvector", "postgis"},
	})
	if err != nil {
		t.Fatalf("RenderTerraformFromBlueprint: %v", err)
	}
	if len(files) != 1 || files[0].Path != "main.tf" {
		t.Fatalf("files = %#v", files)
	}
	body := files[0].Body
	for _, want := range []string{
		`provider "aws"`,
		`resource "aws_db_instance" "agentdb"`,
		`multi_az                = true`,
		`backup_retention_period = 7`,
		`instance_class          = "db.t4g.medium"`,
		`allocated_storage       = 100`,
		`pg_sage_extensions`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("terraform body missing %q:\n%s", want, body)
		}
	}
}

func TestStoreCreatesGeneratedBlueprintAndDraftTemplate(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "bp_store_unit"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")

	blueprint, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{
		BlueprintID: id,
		Name:        "Store unit",
		Provider:    ProviderAWSRDS,
		Intent:      "Private AWS RDS in us-east-1 with Multi-AZ, PITR, 7-day backups and pgvector.",
		CreatedBy:   "unit",
	}, NewHeuristicBlueprintGenerator())
	if err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if blueprint.BlueprintID != id || blueprint.TemplateID != id+"_tf" {
		t.Fatalf("blueprint = %#v", blueprint)
	}
	if blueprint.Status != "generated" || len(blueprint.PolicyFindings) != 0 {
		t.Fatalf("status/findings = %s/%#v", blueprint.Status, blueprint.PolicyFindings)
	}
	templates, err := st.TerraformTemplates(ctx)
	if err != nil {
		t.Fatalf("TerraformTemplates: %v", err)
	}
	var found TerraformTemplate
	for _, template := range templates {
		if template.TemplateID == id+"_tf" {
			found = template
			break
		}
	}
	if found.TemplateID == "" || found.SourceKind != "blueprint" {
		t.Fatalf("generated template not found: %#v", templates)
	}
	if found.Status != "draft" {
		t.Fatalf("generated terraform template status = %q, want draft", found.Status)
	}
	if len(found.Files) != 1 || !strings.Contains(found.Files[0].Body, "aws_db_instance") {
		t.Fatalf("template files = %#v", found.Files)
	}
}

func TestStoreRequiresBlueprintGenerator(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "bp_requires_llm"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)

	_, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{
		BlueprintID: id,
		Name:        "Requires LLM",
		Provider:    ProviderAWSRDS,
		Intent:      "Create AWS RDS in us central 1.",
	}, nil)
	if !errors.Is(err, ErrBlueprintLLMRequired) {
		t.Fatalf("err = %v, want ErrBlueprintLLMRequired", err)
	}
}

func TestProvisionFromBlueprintLinksDeploymentToBlueprintAndTemplate(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	blueprintID := "bp_provision_link"
	deploymentID := "dep_from_bp_link"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", blueprintID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", blueprintID+"_tf")
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", blueprintID)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", blueprintID+"_tf")

	blueprint, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{
		BlueprintID: blueprintID,
		Name:        "Blueprint provision link",
		Provider:    ProviderAWSRDS,
		Intent:      "Private AWS RDS Postgres in us-east-2 with 20GB and 1-day backups.",
		CreatedBy:   "unit",
	}, NewHeuristicBlueprintGenerator())
	if err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if blueprint.Status != "generated" {
		t.Fatalf("blueprint status = %s", blueprint.Status)
	}
	approved, err := st.ApproveBlueprint(ctx, blueprintID, "operator")
	if err != nil {
		t.Fatalf("ApproveBlueprint: %v", err)
	}
	if approved.Status != "approved" || approved.ApprovedBy != "operator" {
		t.Fatalf("approved blueprint = %#v", approved)
	}

	dep, err := st.ProvisionFromBlueprint(ctx, blueprintID, BlueprintProvisionRequest{
		DeploymentID: deploymentID,
		TenantID:     "tenant_agentdb_test",
		AgentID:      "agent_blueprint",
		LeaseSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("ProvisionFromBlueprint: %v", err)
	}
	if dep.DeploymentID != deploymentID || dep.Provider != ProviderAWSRDS {
		t.Fatalf("deployment = %#v", dep)
	}
	if dep.ProvisioningStatus != "planned" || dep.ProvisioningLevel != LevelInstance {
		t.Fatalf("provisioning = %s/%s", dep.ProvisioningStatus, dep.ProvisioningLevel)
	}
	if dep.Metadata["blueprint_id"] != blueprintID ||
		dep.Metadata["terraform_template_id"] != blueprintID+"_tf" {
		t.Fatalf("metadata = %#v", dep.Metadata)
	}
	if dep.ProvisioningPlan["provider"] != ProviderAWSRDS {
		t.Fatalf("plan = %#v", dep.ProvisioningPlan)
	}
}

func TestProvisionFromBlueprintPersistsProviderParamsForLiveRunner(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	blueprintID := "bp_provider_params"
	deploymentID := "dep_bp_provider_params"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", blueprintID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", blueprintID+"_tf")
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", blueprintID)
	defer pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", blueprintID+"_tf")

	blueprint, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{
		BlueprintID: blueprintID,
		Name:        "Provider params blueprint",
		CreatedBy:   "unit",
	}, staticBlueprintGenerator{spec: BlueprintSpec{
		Provider:            ProviderAWSRDS,
		ProvisioningLevel:   LevelInstance,
		Region:              "us-east-2",
		InstanceClass:       "db.t4g.micro",
		StorageGB:           20,
		BackupRetentionDays: 1,
		PrivateNetwork:      true,
	}})
	if err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if _, err := st.ApproveBlueprint(ctx, blueprint.BlueprintID, "operator"); err != nil {
		t.Fatalf("ApproveBlueprint: %v", err)
	}
	dep, err := st.ProvisionFromBlueprint(ctx, blueprint.BlueprintID, BlueprintProvisionRequest{
		DeploymentID: deploymentID,
		TenantID:     "tenant_agentdb_test",
		AgentID:      "agent_blueprint",
		LeaseSeconds: 3600,
		ProviderParams: map[string]any{
			"region":                "us-east-2",
			"db_instance_class":     "db.t4g.micro",
			"allocated_storage":     20,
			"backup_retention_days": 1,
		},
	})
	if err != nil {
		t.Fatalf("ProvisionFromBlueprint: %v", err)
	}
	params, ok := dep.Metadata["provider_params"].(map[string]any)
	if !ok {
		t.Fatalf("provider params missing from metadata: %#v", dep.Metadata)
	}
	if params["region"] != "us-east-2" ||
		float64Param(params, "backup_retention_days") != 1 {
		t.Fatalf("provider params = %#v", params)
	}
}

func TestProvisionFromBlueprintRejectsUnapprovedBlueprint(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	blueprintID := "bp_reject_unapproved"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", blueprintID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", blueprintID+"_tf")

	if _, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{
		BlueprintID: blueprintID,
		Name:        "Reject unapproved",
		Provider:    ProviderGCPCloudSQL,
		Intent:      "Private Cloud SQL Postgres in us-central1.",
	}, NewHeuristicBlueprintGenerator()); err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	_, err := st.ProvisionFromBlueprint(ctx, blueprintID, BlueprintProvisionRequest{
		DeploymentID: "dep_unapproved_bp",
		TenantID:     "tenant_agentdb_test",
		AgentID:      "agent_blueprint",
	})
	if err == nil {
		t.Fatal("expected unapproved blueprint to be rejected")
	}
}

func TestEnsurePromotesCleanDraftBlueprints(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "bp_promote_clean_draft"
	blockedID := "bp_keep_policy_draft"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_blueprints WHERE blueprint_id IN ($1, $2)",
		id, blockedID,
	)

	_, err := pool.Exec(ctx, `
		INSERT INTO sage.agent_db_blueprints (
			blueprint_id, name, status, intent, provider, template_id,
			blueprint_json, policy_findings
		)
		VALUES
			($1, 'Clean draft', 'draft', 'clean', $3, $4, '{}'::jsonb, '[]'::jsonb),
			($2, 'Policy draft', 'draft', 'blocked', $3, $5, '{}'::jsonb,
			 '["public ip is denied"]'::jsonb)`,
		id,
		blockedID,
		ProviderAWSRDS,
		id+"_tf",
		blockedID+"_tf",
	)
	if err != nil {
		t.Fatalf("seed draft blueprints: %v", err)
	}

	if err := st.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	statuses := map[string]string{}
	rows, err := pool.Query(ctx, `
		SELECT blueprint_id, status
		FROM sage.agent_db_blueprints
		WHERE blueprint_id IN ($1, $2)`,
		id,
		blockedID,
	)
	if err != nil {
		t.Fatalf("query statuses: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var blueprintID, status string
		if err := rows.Scan(&blueprintID, &status); err != nil {
			t.Fatalf("scan status: %v", err)
		}
		statuses[blueprintID] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if statuses[id] != "generated" {
		t.Fatalf("clean draft status = %q, want generated", statuses[id])
	}
	if statuses[blockedID] != "draft" {
		t.Fatalf("policy draft status = %q, want draft", statuses[blockedID])
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type staticBlueprintGenerator struct {
	spec BlueprintSpec
}

func (g staticBlueprintGenerator) GenerateBlueprint(
	context.Context,
	BlueprintDraftRequest,
) (BlueprintGeneration, error) {
	files, err := RenderTerraformFromBlueprint(g.spec)
	if err != nil {
		return BlueprintGeneration{}, err
	}
	return BlueprintGeneration{Spec: g.spec, Files: files}, nil
}
