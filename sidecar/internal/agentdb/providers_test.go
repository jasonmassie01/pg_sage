package agentdb

import (
	"strings"
	"testing"
)

func TestBuildProvisionPlanForManagedProviders(t *testing.T) {
	tests := []struct {
		name     string
		req      RegisterRequest
		profile  SizeProfile
		contains []string
	}{
		{
			name: "rds instance",
			req: RegisterRequest{
				DeploymentID:      "adb_rds",
				Provider:          ProviderAWSRDS,
				ProvisioningLevel: LevelInstance,
				DatabaseName:      "agent_app",
			},
			profile: SizeProfile{
				Provider:          ProviderAWSRDS,
				ProvisioningLevel: LevelInstance,
				ProviderParams: map[string]any{
					"region":            "us-east-1",
					"db_instance_class": "db.t4g.micro",
					"engine_version":    "16.3",
				},
				StorageGB: 20,
			},
			contains: []string{
				"terraform", "apply", "provider=aws_rds",
				"db_instance_identifier=adb-rds",
			},
		},
		{
			name: "cloudsql instance",
			req: RegisterRequest{
				DeploymentID:      "adb_gcp",
				Provider:          ProviderGCPCloudSQL,
				ProvisioningLevel: LevelInstance,
				DatabaseName:      "agent_app",
			},
			profile: SizeProfile{
				Provider:          ProviderGCPCloudSQL,
				ProvisioningLevel: LevelInstance,
				ProviderParams: map[string]any{
					"project":      "agent-project",
					"region":       "us-central1",
					"tier":         "db-f1-micro",
					"edition":      "ENTERPRISE",
					"ipv4_enabled": true,
					"require_ssl":  true,
				},
			},
			contains: []string{
				"terraform", "apply", "provider=gcp_cloudsql",
				"instance_name=adb-gcp", "edition=ENTERPRISE",
				"ipv4_enabled=true", "require_ssl=true",
			},
		},
		{
			name: "lakebase branch",
			req: RegisterRequest{
				DeploymentID:      "adb_lakebase",
				Provider:          ProviderDatabricksLakebase,
				ProvisioningLevel: LevelInstance,
				DatabaseName:      "agent_app",
			},
			profile: SizeProfile{
				Provider:          ProviderDatabricksLakebase,
				ProvisioningLevel: LevelInstance,
				ProviderParams: map[string]any{
					"project": "agent-project",
					"mode":    "autoscaling_branch",
				},
			},
			contains: []string{
				"cloud_api", "databricks_lakebase", "create_branch",
				"adb-lakebase",
			},
		},
		{
			name: "lakebase instance override",
			req: RegisterRequest{
				DeploymentID:      "adb_lakebase_instance",
				Provider:          ProviderDatabricksLakebase,
				ProvisioningLevel: LevelInstance,
				DatabaseName:      "agent_app",
				Metadata: map[string]any{
					"lakebase_mode": "provisioned_instance",
				},
			},
			profile: SizeProfile{
				Provider:          ProviderDatabricksLakebase,
				ProvisioningLevel: LevelInstance,
				ProviderParams: map[string]any{
					"project": "agent-project",
					"mode":    "autoscaling_branch",
				},
			},
			contains: []string{
				"cloud_api", "databricks_lakebase", "create_instance",
				"adb-lakebase-instance",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := BuildProvisionPlan(tt.req, tt.profile)
			if err != nil {
				t.Fatalf("BuildProvisionPlan: %v", err)
			}
			joined := strings.Join(plan.Commands[0].Args, " ")
			for _, want := range tt.contains {
				if !strings.Contains(joined, want) {
					t.Fatalf("command %q missing %q", joined, want)
				}
			}
			if plan.ExecutionMode != "manual_review" {
				t.Fatalf("execution mode = %s, want manual_review", plan.ExecutionMode)
			}
			if tt.req.Provider == ProviderDatabricksLakebase &&
				!notesContain(plan.Notes, "extension allowlist") {
				t.Fatalf("lakebase notes should call out extension allowlist: %#v",
					plan.Notes)
			}
		})
	}
}

func TestBuildProvisionPlanRejectsCloudNonInstanceRequests(t *testing.T) {
	_, err := BuildProvisionPlan(RegisterRequest{
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelDatabase,
		DatabaseName:      "agent_app",
	}, SizeProfile{Provider: ProviderAWSRDS, ProvisioningLevel: LevelDatabase})
	if err == nil {
		t.Fatal("expected cloud non-instance error")
	}
}

func TestProviderReadinessUsesAPITerraformInterfaces(t *testing.T) {
	providers := ProviderReadinessList(t.Context())

	expected := map[string]string{
		ProviderAWSRDS:             "terraform_or_aws_sdk",
		ProviderGCPCloudSQL:        "terraform_or_cloudsql_admin_api",
		ProviderDatabricksLakebase: "databricks_api_or_terraform",
	}
	for _, provider := range providers {
		if want := expected[provider.Provider]; want != "" {
			if provider.Interface != want {
				t.Fatalf("%s interface = %q, want %q",
					provider.Provider, provider.Interface, want)
			}
			if provider.CLI != "" {
				t.Fatalf("%s CLI = %q, want empty", provider.Provider, provider.CLI)
			}
			if !provider.Found {
				t.Fatalf("%s should report configured planning interface", provider.Provider)
			}
		}
	}
}

func TestNormalizeProviderAndLevelDefaults(t *testing.T) {
	req := RegisterRequest{IsolationType: "database"}
	normalizeProviderFields(&req)

	if req.Provider != ProviderLocalPostgres {
		t.Fatalf("provider = %s", req.Provider)
	}
	if req.ProvisioningLevel != LevelDatabase || req.IsolationType != LevelDatabase {
		t.Fatalf("level/isolation = %s/%s", req.ProvisioningLevel, req.IsolationType)
	}
}

func TestProvisionRejectsUnsupportedProviderLevelCombinations(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()

	_, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      "bad_cloud_schema",
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_bad",
		Provider:          ProviderAWSRDS,
		ProvisioningLevel: LevelSchema,
	})
	if err != ErrInvalid {
		t.Fatalf("cloud schema err = %v, want ErrInvalid", err)
	}

	_, err = st.Provision(ctx, RegisterRequest{
		DeploymentID:      "bad_local_instance",
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_bad",
		Provider:          ProviderLocalPostgres,
		ProvisioningLevel: LevelInstance,
	})
	if err != ErrInvalid {
		t.Fatalf("local instance err = %v, want ErrInvalid", err)
	}
}

func TestProvisionCloudInstanceUsesDefaultProfile(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_cloud_default_profile"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)

	dep, err := st.Provision(ctx, RegisterRequest{
		DeploymentID:      id,
		TenantID:          "tenant_agentdb_test",
		AgentID:           "agent_cloud",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      60,
	})
	if err != nil {
		t.Fatalf("Provision cloud plan: %v", err)
	}
	if dep.ProvisioningStatus != "planned" {
		t.Fatalf("status = %s, want planned", dep.ProvisioningStatus)
	}
	if dep.ProvisioningPlan["provider"] != ProviderGCPCloudSQL {
		t.Fatalf("plan = %#v", dep.ProvisioningPlan)
	}
}

func notesContain(notes []string, sub string) bool {
	for _, note := range notes {
		if strings.Contains(note, sub) {
			return true
		}
	}
	return false
}
