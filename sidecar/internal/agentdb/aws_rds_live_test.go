package agentdb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestAWSRDSLiveProvisioning(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_AWS_RDS") != "1" {
		t.Skip("set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete")
	}
	region := liveEnvDefault("PG_SAGE_AWS_REGION",
		liveEnvDefault("AWS_REGION", "us-east-2"))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	runner, err := NewAWSRDSRunnerFromDefaultConfig(ctx, region)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	req := awsRunnerRequest(liveDeploymentID("live_aws_rds"))
	req.Policy = LiveProvisionPolicy{AllowedRegions: []string{region}}
	req.Deployment.ProviderResourceID = ""
	params := req.Deployment.Metadata["provider_params"].(map[string]any)
	params["region"] = region
	params["backup_retention_days"] = 1
	params["publicly_accessible"] = false
	params["allocated_storage"] = 20
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
	req.Deployment.ProviderResourceID = create.ProviderResourceID
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
	liveReceipt(t, ProviderAWSRDS, map[string]any{
		"deployment_id":       req.Deployment.DeploymentID,
		"resource_id":         req.Deployment.ProviderResourceID,
		"region":              region,
		"create_seconds":      time.Since(createStart).Seconds(),
		"available_seconds":   statusTime.Seconds(),
		"delete_seconds":      time.Since(destroyStart).Seconds(),
		"gone_seconds":        deleteTime.Seconds(),
		"class":               params["db_instance_class"],
		"storage_gb":          params["allocated_storage"],
		"backup_retention":    params["backup_retention_days"],
		"cleanup_confirmed":   true,
		"cost_guard":          "db.t4g.micro, 20 GiB, ttl tag, delete verified",
		"publicly_accessible": false,
	})
}
