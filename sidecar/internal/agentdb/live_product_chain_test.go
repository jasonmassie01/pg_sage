package agentdb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAgentDBLiveGauntletBlueprintToAWSRDS(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_AGENTDB_GAUNTLET") != "1" ||
		os.Getenv("PG_SAGE_LIVE_AWS_RDS") != "1" {
		t.Skip("set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1")
	}
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	region := liveEnvDefault("PG_SAGE_AWS_REGION",
		liveEnvDefault("AWS_REGION", "us-east-2"))
	runner, err := NewAWSRDSRunnerFromDefaultConfig(ctx, region)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	deploymentID := liveDeploymentID("chain_aws_bp")
	blueprintID := "bp_" + deploymentID
	cleanupDeploymentRows(t, pool, ctx, deploymentID)
	dep, err := createAWSBlueprintDeployment(ctx, st, blueprintID, deploymentID, region)
	if err != nil {
		t.Fatalf("create blueprint deployment: %v", err)
	}
	runLiveProductChain(t, st, dep, runner, LiveProvisionPolicy{
		AllowedRegions: []string{region},
	}, map[string]any{
		"chain_source": "blueprint",
		"region":       region,
	})
}

func TestAgentDBLiveGauntletTerraformTemplateToCloudSQL(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_AGENTDB_GAUNTLET") != "1" ||
		os.Getenv("PG_SAGE_LIVE_GCP_CLOUDSQL") != "1" {
		t.Skip("set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1")
	}
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	project := liveEnvDefault("PG_SAGE_GCP_PROJECT",
		liveEnvDefault("GOOGLE_CLOUD_PROJECT", liveEnv("GCLOUD_PROJECT")))
	if project == "" {
		t.Fatal("PG_SAGE_GCP_PROJECT or GOOGLE_CLOUD_PROJECT is required")
	}
	token := requireLiveEnv(t, "PG_SAGE_GCP_ACCESS_TOKEN")
	region := liveEnvDefault("PG_SAGE_GCP_REGION", "us-central1")
	runner := NewCloudSQLRunner(CloudSQLHTTPClient{
		TokenFunc: func(context.Context) (string, error) { return token, nil },
	}, project, region)
	deploymentID := liveDeploymentID("chain_gcp_tf")
	templateID := "tf_" + deploymentID
	cleanupDeploymentRows(t, pool, ctx, deploymentID)
	dep, err := createCloudSQLTemplateDeployment(
		ctx, st, templateID, deploymentID, project, region,
	)
	if err != nil {
		t.Fatalf("create template deployment: %v", err)
	}
	runLiveProductChain(t, st, dep, runner, LiveProvisionPolicy{
		AllowedRegions: []string{region},
		AllowPublicIP:  true,
	}, map[string]any{
		"chain_source": "terraform_template",
		"project":      project,
		"region":       region,
	})
}

func TestAgentDBLiveGauntletAgentRequestToLakebaseBranch(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_AGENTDB_GAUNTLET") != "1" ||
		os.Getenv("PG_SAGE_LIVE_DATABRICKS_LAKEBASE") != "1" {
		t.Skip("set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1")
	}
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	host := liveEnvDefault("PG_SAGE_DATABRICKS_HOST", liveEnv("DATABRICKS_HOST"))
	if host == "" {
		t.Fatal("PG_SAGE_DATABRICKS_HOST or DATABRICKS_HOST is required")
	}
	token := liveEnvDefault("PG_SAGE_DATABRICKS_TOKEN", liveEnv("DATABRICKS_TOKEN"))
	if token == "" {
		t.Fatal("PG_SAGE_DATABRICKS_TOKEN or DATABRICKS_TOKEN is required")
	}
	project := requireLiveEnv(t, "PG_SAGE_LIVE_LAKEBASE_PROJECT")
	source := requireLiveEnv(t, "PG_SAGE_LIVE_LAKEBASE_SOURCE_INSTANCE")
	runner := NewLakebaseRunner(LakebaseHTTPClient{
		BaseURL:   host,
		TokenFunc: func(context.Context) (string, error) { return token, nil },
	}, false)
	deploymentID := liveDeploymentID("chain_lake_req")
	requestID := "req_" + deploymentID
	cleanupDeploymentRows(t, pool, ctx, deploymentID)
	dep, err := createLakebaseRequestDeployment(
		ctx, st, requestID, deploymentID, project, source,
	)
	if err != nil {
		t.Fatalf("create agent request deployment: %v", err)
	}
	runLiveProductChain(t, st, dep, runner, LiveProvisionPolicy{}, map[string]any{
		"chain_source":    "agent_request",
		"project":         project,
		"source_instance": source,
	})
}

func createAWSBlueprintDeployment(
	ctx context.Context,
	st *Store,
	blueprintID string,
	deploymentID string,
	region string,
) (Deployment, error) {
	blueprint, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{
		BlueprintID: blueprintID,
		Name:        "Live AWS blueprint gauntlet",
		CreatedBy:   "live-gauntlet",
	}, staticBlueprintGenerator{spec: BlueprintSpec{
		Provider:            ProviderAWSRDS,
		ProvisioningLevel:   LevelInstance,
		Region:              region,
		InstanceClass:       "db.t4g.micro",
		StorageGB:           20,
		BackupRetentionDays: 1,
		PrivateNetwork:      true,
	}})
	if err != nil {
		return Deployment{}, err
	}
	if _, err := st.ApproveBlueprint(ctx, blueprint.BlueprintID, "operator"); err != nil {
		return Deployment{}, err
	}
	return st.ProvisionFromBlueprint(ctx, blueprint.BlueprintID, BlueprintProvisionRequest{
		DeploymentID: deploymentID,
		TenantID:     "tenant_live_gauntlet",
		AgentID:      "agent_blueprint",
		DatabaseName: "agent_app",
		LeaseSeconds: 3600,
		Metadata:     map[string]any{"disposable": true},
		ProviderParams: map[string]any{
			"region":                region,
			"db_instance_class":     "db.t4g.micro",
			"allocated_storage":     20,
			"backup_retention_days": 1,
			"publicly_accessible":   false,
		},
	})
}

func createCloudSQLTemplateDeployment(
	ctx context.Context,
	st *Store,
	templateID string,
	deploymentID string,
	project string,
	region string,
) (Deployment, error) {
	if _, err := st.CreateTerraformTemplate(ctx, TerraformTemplateRequest{
		TemplateID: templateID,
		Name:       "Live Cloud SQL template gauntlet",
		SourceKind: "upload",
		Files: []TerraformFile{{
			Path: "main.tf",
			Body: `provider "google" {}
resource "google_sql_database_instance" "agentdb" {}`,
		}},
		CreatedBy: "live-gauntlet",
	}); err != nil {
		return Deployment{}, err
	}
	if _, err := st.ApproveTerraformTemplate(ctx, templateID, "operator"); err != nil {
		return Deployment{}, err
	}
	return st.ProvisionFromTerraformTemplate(ctx, templateID, TemplateProvisionRequest{
		DeploymentID:      deploymentID,
		TenantID:          "tenant_live_gauntlet",
		AgentID:           "agent_template",
		Provider:          ProviderGCPCloudSQL,
		ProvisioningLevel: LevelInstance,
		DatabaseName:      "agent_app",
		LeaseSeconds:      3600,
		BudgetUSD:         5,
		Metadata:          map[string]any{"disposable": true},
		ProviderParams: map[string]any{
			"project":             project,
			"region":              region,
			"database_version":    "POSTGRES_16",
			"tier":                liveEnvDefault("PG_SAGE_GCP_CLOUDSQL_TIER", "db-f1-micro"),
			"edition":             liveEnvDefault("PG_SAGE_GCP_CLOUDSQL_EDITION", "ENTERPRISE"),
			"storage_size":        20,
			"ipv4_enabled":        true,
			"authorized_networks": []any{},
		},
	})
}

func createLakebaseRequestDeployment(
	ctx context.Context,
	st *Store,
	requestID string,
	deploymentID string,
	project string,
	source string,
) (Deployment, error) {
	req, err := st.CreateRequest(ctx, RequestCreate{
		RequestID:      requestID,
		TenantID:       "tenant_live_gauntlet",
		AgentID:        "agent_request",
		RunID:          "run_live",
		Purpose:        "live Lakebase branch gauntlet",
		IsolationType:  LevelInstance,
		DatabaseName:   "agent_app",
		Provider:       ProviderDatabricksLakebase,
		BudgetUSD:      1,
		BackupRequired: true,
		Body: map[string]any{
			"tenant_id":                "tenant_live_gauntlet",
			"agent_id":                 "agent_request",
			"provider":                 ProviderDatabricksLakebase,
			"requested_isolation_type": LevelInstance,
		},
	})
	if err != nil {
		return Deployment{}, err
	}
	if req.Status != "approved" {
		if _, err := st.SetRequestDecision(ctx, requestID, DecisionRequest{
			Decision: "approved",
			Reason:   "live gauntlet",
		}); err != nil {
			return Deployment{}, err
		}
	}
	return st.ProvisionApprovedRequest(ctx, requestID, RequestProvisionRequest{
		DeploymentID: deploymentID,
		LeaseSeconds: 3600,
		Metadata: map[string]any{
			"disposable":    true,
			"lakebase_mode": "autoscaling_branch",
		},
		ProviderParams: map[string]any{
			"project":         project,
			"source_instance": source,
			"source_branch":   source,
			"mode":            "autoscaling_branch",
		},
	})
}

func runLiveProductChain(
	t *testing.T,
	st *Store,
	dep Deployment,
	runner ProviderRunner,
	policy LiveProvisionPolicy,
	extraReceipt map[string]any,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	created := false
	t.Cleanup(func() {
		if !created {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cleanupCancel()
		current, err := st.Get(cleanupCtx, dep.DeploymentID)
		if err == nil {
			_ = runner.Destroy(cleanupCtx, ProvisionRequest{
				Operation:  ProvisionOpDestroy,
				Deployment: current,
				Plan:       current.ProvisioningPlan,
			})
		}
	})
	preflight, err := st.PreflightProvision(ctx, dep.DeploymentID)
	if err != nil || preflight.Status != "passed" {
		t.Fatalf("preflight attempt=%#v err=%v", preflight, err)
	}
	createStart := time.Now()
	execute, err := st.ExecuteProvisionLive(ctx, dep.DeploymentID, runner, LiveExecutionRequest{
		Mode:           "live",
		CostEstimateID: "live-gauntlet-" + dep.DeploymentID,
		Policy:         policy,
	})
	if err != nil || execute.Status != "succeeded" {
		t.Fatalf("execute live attempt=%#v err=%v", execute, err)
	}
	created = true
	availableSeconds := waitDeploymentAvailable(t, ctx, st, dep.DeploymentID, runner)
	if _, err := st.Ping(ctx, dep.DeploymentID, PingRequest{
		Status: "active",
		Metrics: map[string]any{
			"source": "live_gauntlet",
			"qps":    1,
		},
	}); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := st.AddCostSample(ctx, dep.DeploymentID, CostSampleRequest{
		CostUSD: 0.01,
		Metric:  "live_gauntlet_estimate",
		Value:   1,
		Unit:    "resource",
	}); err != nil {
		t.Fatalf("AddCostSample: %v", err)
	}
	backup, err := st.CheckBackupAssuranceLive(ctx, dep.DeploymentID, runner)
	if err != nil || !backup.SafeForDestroy {
		t.Fatalf("backup assurance=%#v err=%v", backup, err)
	}
	assertCreationReceipt(t, st, dep.DeploymentID)
	destroyStart := time.Now()
	destroy, err := st.DestroyProvisionLive(ctx, dep.DeploymentID, runner)
	if err != nil || destroy.Status != "succeeded" {
		t.Fatalf("destroy live attempt=%#v err=%v", destroy, err)
	}
	current, err := st.Get(ctx, dep.DeploymentID)
	if err != nil {
		t.Fatalf("Get after destroy: %v", err)
	}
	gone, goneSeconds, err := waitProvisionStatus(ctx, 30*time.Second,
		func() ProvisionResult {
			return runner.Status(ctx, ProvisionRequest{
				Operation:  ProvisionOpStatus,
				Deployment: current,
				Plan:       current.ProvisioningPlan,
			})
		},
		func(result ProvisionResult) bool {
			return errors.Is(publicProviderError(result.Error), ErrNotFound)
		},
	)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("wait deleted err=%v last=%#v", err, gone)
	}
	if !errors.Is(publicProviderError(gone.Error), ErrNotFound) {
		t.Fatalf("delete not confirmed last=%#v", gone)
	}
	if _, err := st.CheckProvisionStatusLive(ctx, dep.DeploymentID, runner); err != nil {
		t.Fatalf("mark destroyed after provider deletion: %v", err)
	}
	current, err = st.Get(ctx, dep.DeploymentID)
	if err != nil {
		t.Fatalf("Get after destroyed status: %v", err)
	}
	created = false
	assertAttemptKinds(t, st, dep.DeploymentID,
		"preflight", "execute_live", "status_check_live",
		"backup_check_live", "destroy_live")
	receipt := map[string]any{
		"deployment_id":       dep.DeploymentID,
		"resource_id":         current.ProviderResourceID,
		"create_seconds":      time.Since(createStart).Seconds(),
		"available_seconds":   availableSeconds,
		"delete_seconds":      time.Since(destroyStart).Seconds(),
		"gone_seconds":        goneSeconds.Seconds(),
		"cleanup_confirmed":   true,
		"product_chain":       true,
		"cost_guard":          "disposable live gauntlet resource, delete verified",
		"backup_status":       backup.BackupStatus,
		"provisioning_status": current.ProvisioningStatus,
	}
	for key, value := range extraReceipt {
		receipt[key] = value
	}
	liveReceipt(t, dep.Provider, receipt)
}

func waitDeploymentAvailable(
	t *testing.T,
	ctx context.Context,
	st *Store,
	id string,
	runner ProviderRunner,
) float64 {
	t.Helper()
	start := time.Now()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		dep, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get while waiting: %v", err)
		}
		if dep.ProvisioningStatus == "available" {
			return time.Since(start).Seconds()
		}
		_, err = st.CheckProvisionStatusLive(ctx, id, runner)
		if err != nil {
			t.Fatalf("CheckProvisionStatusLive: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait available timed out: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertCreationReceipt(t *testing.T, st *Store, deploymentID string) {
	t.Helper()
	var providerResourceID string
	if err := st.pool.QueryRow(context.Background(), `
		SELECT provider_resource_id
		FROM sage.agent_db_creation_receipts
		WHERE deployment_id=$1`, deploymentID).Scan(&providerResourceID); err != nil {
		t.Fatalf("creation receipt missing: %v", err)
	}
	if providerResourceID == "" {
		t.Fatalf("creation receipt missing provider resource id")
	}
}

func assertAttemptKinds(t *testing.T, st *Store, deploymentID string, kinds ...string) {
	t.Helper()
	attempts, err := st.ProvisionAttempts(context.Background(), deploymentID)
	if err != nil {
		t.Fatalf("ProvisionAttempts: %v", err)
	}
	seen := map[string]bool{}
	for _, attempt := range attempts {
		seen[attempt.Kind] = true
	}
	for _, kind := range kinds {
		if !seen[kind] {
			t.Fatalf("attempt kind %q missing from %#v", kind, attempts)
		}
	}
}

func cleanupDeploymentRows(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	deploymentID string,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		DELETE FROM sage.agent_db_deployments
		WHERE deployment_id=$1`, deploymentID)
	if err != nil {
		t.Logf("cleanup deployment rows: %v", err)
	}
}
