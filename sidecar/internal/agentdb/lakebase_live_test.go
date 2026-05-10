package agentdb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestLakebaseLiveProvisioning(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_DATABRICKS_LAKEBASE") != "1" {
		t.Skip("set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle")
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	client := LakebaseHTTPClient{
		BaseURL: host,
		TokenFunc: func(context.Context) (string, error) {
			return token, nil
		},
	}
	runner := NewLakebaseRunner(client, false)
	req := lakebaseRequest(liveDeploymentID("live_lakebase"))
	req.Deployment.ProviderResourceID = ""
	params := req.Deployment.Metadata["provider_params"].(map[string]any)
	params["project"] = project
	params["source_instance"] = source
	params["source_branch"] = source
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
			context.Background(), 10*time.Minute,
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
	status, statusTime, err := waitProvisionStatus(ctx, 15*time.Second,
		func() ProvisionResult { return runner.Status(ctx, req) },
		func(result ProvisionResult) bool { return result.Status == "available" },
	)
	if err != nil {
		t.Fatalf("wait available err=%v last=%#v", err, status)
	}
	destroyStart := time.Now()
	destroy := runner.Destroy(ctx, req)
	if destroy.Error != nil {
		t.Fatalf("destroy = %#v", destroy)
	}
	gone, deleteTime, err := waitProvisionStatus(ctx, 15*time.Second,
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
	liveReceipt(t, ProviderDatabricksLakebase, map[string]any{
		"deployment_id":     req.Deployment.DeploymentID,
		"resource_id":       req.Deployment.ProviderResourceID,
		"project":           project,
		"source_instance":   source,
		"create_seconds":    time.Since(createStart).Seconds(),
		"available_seconds": statusTime.Seconds(),
		"delete_seconds":    time.Since(destroyStart).Seconds(),
		"gone_seconds":      deleteTime.Seconds(),
		"cleanup_confirmed": true,
		"cost_guard":        "branch ttl, source branch explicit, delete verified",
	})
}
