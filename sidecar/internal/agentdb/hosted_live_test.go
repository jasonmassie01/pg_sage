//go:build providerlive

package agentdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestHostedLiveLifecycle(t *testing.T) {
	provider := os.Getenv("PG_SAGE_HOSTED_TEST_PROVIDER")
	token := os.Getenv("PG_SAGE_HOSTED_TEST_TOKEN")
	if token == "" || os.Getenv("PG_SAGE_HOSTED_TEST_OWNED") != "1" {
		t.Fatal("explicit disposable provider credentials and ownership guard required")
	}
	mode, scope := "branch", os.Getenv("PG_SAGE_HOSTED_TEST_PROJECT")
	if provider == ProviderSupabase {
		mode, scope = "project", os.Getenv("PG_SAGE_HOSTED_TEST_ORG")
	}
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	client := HostedHTTPClient{Provider: provider, TokenFunc: staticToken(token),
		PasswordFunc: staticToken(hex.EncodeToString(secret))}
	params := map[string]any{"mode": mode, "source_branch": os.Getenv("PG_SAGE_HOSTED_TEST_SOURCE")}
	params["region"] = os.Getenv("PG_SAGE_HOSTED_TEST_REGION")
	if mode == "branch" {
		params["project"] = scope
	} else {
		params["organization"] = scope
	}
	req := ProvisionRequest{Deployment: Deployment{Provider: provider,
		ProvisioningLevel: LevelInstance,
		DeploymentID:      "compat-" + time.Now().UTC().Format("20060102-150405"),
		Metadata:          map[string]any{"provider_params": params}}}
	runner := NewHostedRunner(provider, client)
	runHostedLiveLifecycle(t, runner, req)
}

func runHostedLiveLifecycle(t *testing.T, runner HostedRunner, req ProvisionRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if result := runner.Preflight(ctx, req); result.Error != nil {
		t.Fatal(result.Error)
	}
	created := runner.Create(ctx, req)
	if created.Error != nil {
		t.Fatal(created.Error)
	}
	if created.ProviderResourceID == "" {
		t.Fatal("created resource ID missing")
	}
	req.Deployment.ProviderResourceID = created.ProviderResourceID
	t.Cleanup(func() { cleanupHostedLive(t, runner, req) })
	for {
		status := runner.Status(ctx, req)
		if status.Error != nil {
			t.Fatal(status.Error)
		}
		if status.Status == "available" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("provider did not become available")
		case <-time.After(3 * time.Second):
		}
	}
	if result := runner.BackupCheck(ctx, req); result.Detail["restore_verified"] == true {
		t.Fatal("status-only backup check claimed restored data")
	}
	t.Log("CHECK: live create and available status verified; cleanup follows")
}

func cleanupHostedLive(t *testing.T, runner HostedRunner, req ProvisionRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := runner.Destroy(ctx, req)
	if result.Error != nil {
		t.Errorf("task resource cleanup failed: %v", result.Error)
		return
	}
	params := providerParams(req.Deployment)
	mode := stringParam(params, "mode")
	scope := firstNonEmpty(stringParam(params, "project"), stringParam(params, "organization"))
	for {
		_, err := runner.client.GetResource(ctx, mode, scope, req.Deployment.ProviderResourceID)
		if pe, ok := err.(ProviderError); ok && pe.Kind == ProviderErrNotFound {
			t.Log("CHECK: provider independently confirms resource deleted")
			return
		}
		select {
		case <-ctx.Done():
			t.Error("task resource deletion not confirmed")
			return
		case <-time.After(3 * time.Second):
		}
	}
}
