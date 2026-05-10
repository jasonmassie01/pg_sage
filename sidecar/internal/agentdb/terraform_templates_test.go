package agentdb

import "testing"

func TestTerraformTemplatePolicy(t *testing.T) {
	valid := ValidateTerraformTemplate(TerraformTemplateRequest{
		Name: "valid",
		Files: []TerraformFile{{
			Path: "main.tf",
			Body: `provider "aws" {}
resource "aws_db_instance" "db" {}
variable "region" {}
output "endpoint" { value = "x" }`,
		}},
	})
	if valid.Status != "draft" || len(valid.PolicyFindings) != 0 {
		t.Fatalf("valid template rejected: %#v", valid)
	}
	if len(valid.Manifest["resources"].([]string)) != 1 {
		t.Fatalf("manifest resources = %#v", valid.Manifest)
	}

	bad := ValidateTerraformTemplate(TerraformTemplateRequest{
		Name: "bad",
		Files: []TerraformFile{
			{Path: "secrets.auto.tfvars", Body: `password = "x"`},
			{Path: "main.tf", Body: `resource "null_resource" "x" { provisioner "local-exec" { command = "bad" } }`},
		},
	})
	if bad.Status != "rejected" || len(bad.PolicyFindings) < 2 {
		t.Fatalf("bad template not rejected: %#v", bad)
	}
}

func TestTerraformTemplateStore(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1",
		"tf_unit",
	)
	template, err := st.CreateTerraformTemplate(ctx, TerraformTemplateRequest{
		TemplateID: "tf_unit",
		Name:       "unit",
		Files:      []TerraformFile{{Path: "main.tf", Body: `resource "aws_db_instance" "db" {}`}},
		CreatedBy:  "test",
	})
	if err != nil {
		t.Fatalf("CreateTerraformTemplate: %v", err)
	}
	if template.TemplateID != "tf_unit" || template.Status != "draft" {
		t.Fatalf("template = %#v", template)
	}
	approved, err := st.ApproveTerraformTemplate(ctx, "tf_unit", "operator")
	if err != nil {
		t.Fatalf("ApproveTerraformTemplate: %v", err)
	}
	if approved.Status != "approved" || approved.ApprovedBy != "operator" {
		t.Fatalf("approved = %#v", approved)
	}
}

func TestProvisionFromTerraformTemplateLinksDeployment(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	templateID := "tf_provision_link"
	deploymentID := "dep_from_tf_link"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", templateID)

	template, err := st.CreateTerraformTemplate(ctx, TerraformTemplateRequest{
		TemplateID: templateID,
		Name:       "Template provision link",
		SourceKind: "upload",
		Files: []TerraformFile{{
			Path: "main.tf",
			Body: `provider "aws" {}
resource "aws_db_instance" "agentdb" { engine = "postgres" }`,
		}},
		CreatedBy: "unit",
	})
	if err != nil {
		t.Fatalf("CreateTerraformTemplate: %v", err)
	}
	if template.Status != "draft" {
		t.Fatalf("template status = %s", template.Status)
	}
	if _, err := st.ApproveTerraformTemplate(ctx, templateID, "operator"); err != nil {
		t.Fatalf("ApproveTerraformTemplate: %v", err)
	}

	dep, err := st.ProvisionFromTerraformTemplate(ctx, templateID, TemplateProvisionRequest{
		DeploymentID:      deploymentID,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_template",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      3600,
	})
	if err != nil {
		t.Fatalf("ProvisionFromTerraformTemplate: %v", err)
	}
	if dep.Metadata["terraform_template_id"] != templateID {
		t.Fatalf("metadata = %#v", dep.Metadata)
	}
	if dep.ProvisioningPlan["source"] != "terraform_template" {
		t.Fatalf("plan = %#v", dep.ProvisioningPlan)
	}
	if dep.ProvisioningStatus != "planned" {
		t.Fatalf("status = %s", dep.ProvisioningStatus)
	}
}

func TestProvisionFromTerraformTemplatePersistsProviderParamsForLiveRunner(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	templateID := "tf_provider_params"
	deploymentID := "dep_tf_provider_params"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", deploymentID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", templateID)

	if _, err := st.CreateTerraformTemplate(ctx, TerraformTemplateRequest{
		TemplateID: templateID,
		Name:       "Provider params template",
		SourceKind: "upload",
		Files: []TerraformFile{{
			Path: "main.tf",
			Body: `provider "google" {}
resource "google_sql_database_instance" "agentdb" {}`,
		}},
		CreatedBy: "unit",
	}); err != nil {
		t.Fatalf("CreateTerraformTemplate: %v", err)
	}
	if _, err := st.ApproveTerraformTemplate(ctx, templateID, "operator"); err != nil {
		t.Fatalf("ApproveTerraformTemplate: %v", err)
	}
	dep, err := st.ProvisionFromTerraformTemplate(ctx, templateID, TemplateProvisionRequest{
		DeploymentID:      deploymentID,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_template",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      3600,
		ProviderParams: map[string]any{
			"project":             "demo-project",
			"region":              "us-central1",
			"database_version":    "POSTGRES_16",
			"tier":                "db-f1-micro",
			"edition":             "ENTERPRISE",
			"storage_size":        20,
			"ipv4_enabled":        true,
			"authorized_networks": []any{},
		},
	})
	if err != nil {
		t.Fatalf("ProvisionFromTerraformTemplate: %v", err)
	}
	params, ok := dep.Metadata["provider_params"].(map[string]any)
	if !ok {
		t.Fatalf("provider params missing from metadata: %#v", dep.Metadata)
	}
	if params["project"] != "demo-project" || params["tier"] != "db-f1-micro" {
		t.Fatalf("provider params = %#v", params)
	}
}
