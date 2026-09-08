package agentdb

import "fmt"

func hostedTerraformHeader(spec BlueprintSpec) string {
	source, version := "kislerdm/neon", "0.15.0"
	if spec.Provider == ProviderSupabase {
		source, version = "supabase/supabase", "1.10.1"
	}
	return fmt.Sprintf(`terraform {
  required_providers {
    %s = {
      source = %q
      version = %q
    }
  }
}
# Configure provider credentials through its documented environment variable.
# Keep Terraform state encrypted and access restricted; provider state can contain credentials.
variable "organization" {
  type = string
}
variable "name" {
  type = string
}
`, spec.Provider, source, version)
}

func renderHostedProject(spec BlueprintSpec) string {
	header := hostedTerraformHeader(spec)
	if spec.Provider == ProviderNeon {
		return header + fmt.Sprintf(`resource "neon_project" "database" {
  name = var.name
  org_id = var.organization
  region_id = %q
  default_endpoint_settings {
    autoscaling_limit_min_cu = 0.25
    autoscaling_limit_max_cu = 0.25
  }
}

`, spec.Region)
	}
	return header + fmt.Sprintf(`variable "database_password" {
  type = string
  sensitive = true
}
resource "supabase_project" "database" {
  name = var.name
  organization_id = var.organization
  database_password = var.database_password
  region = %q
}
`, spec.Region)
}

func renderHostedBranch(spec BlueprintSpec) string {
	header := hostedTerraformHeader(spec) + `variable "parent_project" {
  type = string
}
`
	if spec.Provider == ProviderNeon {
		return header + `# Terraform Neon branches copy parent data; review isolation before approval.
variable "parent_branch" {
  type = string
}
resource "neon_branch" "database" {
  project_id = var.parent_project
  parent_id = var.parent_branch
  name = var.name
}
resource "neon_endpoint" "database" {
  project_id = var.parent_project
  branch_id = neon_branch.database.id
  type = "read_write"
  autoscaling_limit_min_cu = 0.25
  autoscaling_limit_max_cu = 0.25
}
`
	}
	return header + `# Verify organization branching entitlement and billing before approval.
resource "supabase_branch" "database" {
  parent_project_ref = var.parent_project
  git_branch = var.name
  persistent = false
}
`
}

func hostedBlueprintFindings(spec BlueprintSpec) []string {
	if spec.Provider != ProviderNeon && spec.Provider != ProviderSupabase {
		return nil
	}
	findings := []string{}
	if spec.PrivateNetwork || spec.MultiAZ || spec.PITR || spec.BackupRetentionDays > 0 {
		findings = append(findings,
			"requested network, HA or backup controls are not configured by this hosted template")
	}
	if spec.HostedMode == "branch" {
		findings = append(findings,
			"review branch data isolation and organization entitlement before approval")
	}
	if spec.StorageGB > 0 || spec.DatabaseVersion != "" || spec.InstanceClass != "" {
		findings = append(findings,
			"requested storage, database version or compute controls are not configured by this template")
	}
	if len(spec.Extensions) > 0 {
		findings = append(findings, "requested extensions require a separate database migration")
	}
	return findings
}
