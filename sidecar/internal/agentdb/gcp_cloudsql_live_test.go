package agentdb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCloudSQLLiveProvisioning(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_GCP_CLOUDSQL") != "1" {
		t.Skip("set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete")
	}
	project := liveEnvDefault("PG_SAGE_GCP_PROJECT",
		liveEnvDefault("GOOGLE_CLOUD_PROJECT", liveEnv("GCLOUD_PROJECT")))
	if project == "" {
		t.Fatal("PG_SAGE_GCP_PROJECT or GOOGLE_CLOUD_PROJECT is required")
	}
	token := requireLiveEnv(t, "PG_SAGE_GCP_ACCESS_TOKEN")
	region := liveEnvDefault("PG_SAGE_GCP_REGION", "us-central1")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	client := CloudSQLHTTPClient{
		TokenFunc: func(context.Context) (string, error) {
			return token, nil
		},
	}
	runner := NewCloudSQLRunner(client, project, region)
	req := cloudSQLRequest(liveDeploymentID("live_gcp_cloudsql"))
	req.Policy = LiveProvisionPolicy{
		AllowedRegions: []string{region},
		AllowPublicIP:  true,
	}
	req.Deployment.ProviderResourceID = ""
	params := req.Deployment.Metadata["provider_params"].(map[string]any)
	params["project"] = project
	params["region"] = region
	params["database_version"] = "POSTGRES_16"
	params["tier"] = liveEnvDefault("PG_SAGE_GCP_CLOUDSQL_TIER", "db-f1-micro")
	params["edition"] = liveEnvDefault("PG_SAGE_GCP_CLOUDSQL_EDITION", "ENTERPRISE")
	params["storage_size"] = 20
	params["ipv4_enabled"] = true
	params["authorized_networks"] = []any{}
	preflight := runner.Preflight(ctx, req)
	if preflight.Status != "preflight_passed" || preflight.Error != nil {
		t.Fatalf("preflight = %#v", preflight)
	}
	resourceID := preflight.ProviderResourceID
	created := false
	t.Cleanup(func() {
		if !created {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(), 20*time.Minute,
		)
		defer cleanupCancel()
		req.Deployment.ProviderResourceID = resourceID
		_ = runner.Destroy(cleanupCtx, req)
	})
	createStart := time.Now()
	create := runner.Create(ctx, req)
	if create.Error != nil {
		t.Fatalf("create = %#v", create)
	}
	created = true
	req.Deployment.ProviderResourceID = resourceID
	status, statusTime, err := waitProvisionStatus(ctx, 30*time.Second,
		func() ProvisionResult { return runner.Status(ctx, req) },
		func(result ProvisionResult) bool { return result.Status == "available" },
	)
	if err != nil {
		t.Fatalf("wait available err=%v last=%#v", err, status)
	}
	backup := runner.BackupCheck(ctx, req)
	if backup.Error != nil || backup.Status != "available" {
		t.Fatalf("backup/status check = %#v", backup)
	}
	destroyStart := time.Now()
	destroy := runner.Destroy(ctx, req)
	if destroy.Error != nil {
		t.Fatalf("destroy = %#v", destroy)
	}
	gone, deleteTime, err := waitProvisionStatus(ctx, 30*time.Second,
		func() ProvisionResult { return runner.Status(ctx, req) },
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
	created = false
	liveReceipt(t, ProviderGCPCloudSQL, map[string]any{
		"deployment_id":       req.Deployment.DeploymentID,
		"resource_id":         req.Deployment.ProviderResourceID,
		"project":             project,
		"region":              region,
		"create_seconds":      time.Since(createStart).Seconds(),
		"available_seconds":   statusTime.Seconds(),
		"delete_seconds":      time.Since(destroyStart).Seconds(),
		"gone_seconds":        deleteTime.Seconds(),
		"tier":                params["tier"],
		"storage_gb":          params["storage_size"],
		"backup_enabled":      true,
		"cleanup_confirmed":   true,
		"cost_guard":          "db-f1-micro, 20 GiB, disposable label, delete verified",
		"publicly_accessible": true,
		"authorized_networks": 0,
	})
}
