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
			contains: []string{"aws", "rds", "create-db-instance", "adb-rds"},
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
					"project": "agent-project",
					"region":  "us-central1",
					"tier":    "db-custom-2-8192",
				},
			},
			contains: []string{
				"gcloud", "sql", "instances", "create", "adb-gcp",
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
				"databricks", "database", "branches", "create", "adb-lakebase",
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
