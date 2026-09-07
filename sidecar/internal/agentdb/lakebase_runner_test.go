package agentdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLakebaseRunnerBranchLifecycle(t *testing.T) {
	runner := NewLakebaseRunner(&fakeLakebaseClient{}, false)
	req := lakebaseRequest("lakebase_runner")
	preflight := runner.Preflight(context.Background(), req)
	if preflight.Status != "preflight_passed" ||
		preflight.ProviderResourceID != "pgsage-lakebase-runner" {
		t.Fatalf("preflight = %#v", preflight)
	}
	created := runner.Create(context.Background(), req)
	if created.Status != "available" || created.ProviderResourceID == "" {
		t.Fatalf("created = %#v", created)
	}
	status := runner.Status(context.Background(), req)
	if status.Status != "available" {
		t.Fatalf("status = %#v", status)
	}
	destroy := runner.Destroy(context.Background(), req)
	if destroy.Status != "destroying" {
		t.Fatalf("destroy = %#v", destroy)
	}
}

func TestLakebaseRunnerRejectsInstanceMode(t *testing.T) {
	req := lakebaseRequest("lakebase_instance")
	req.Deployment.ProvisioningPlan = map[string]any{
		"commands": []any{
			map[string]any{"args": []any{"cloud_api", "databricks_lakebase", "create_instance"}},
		},
	}
	result := NewLakebaseRunner(&fakeLakebaseClient{}, false).
		Preflight(context.Background(), req)
	if result.Error == nil {
		t.Fatal("expected instance-mode rejection")
	}
}

func TestLakebaseRunnerUsesSourceInstanceForBranches(t *testing.T) {
	client := &fakeLakebaseClient{}
	runner := NewLakebaseRunner(client, false)
	req := lakebaseRequest("lakebase_source")
	params := req.Deployment.Metadata["provider_params"].(map[string]any)
	params["source_instance"] = "projects/demo/branches/customer-main"

	result := runner.Create(context.Background(), req)
	if result.Status != "available" {
		t.Fatalf("create with source instance = %#v", result)
	}
	if client.created.SourceBranch != "projects/demo/branches/customer-main" {
		t.Fatalf("source branch = %q", client.created.SourceBranch)
	}
}

func TestLakebaseHTTPClientDoesNotPersistToken(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/2.0/postgres/projects/p/branches/b" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(
			`{"name":"projects/p/branches/b","status":{"current_state":"READY"}}`,
		))
	}))
	defer server.Close()
	client := LakebaseHTTPClient{
		BaseURL: server.URL,
		TokenFunc: func(context.Context) (string, error) {
			return "fresh-token", nil
		},
		HTTPClient: server.Client(),
	}
	got, err := client.GetBranch(context.Background(), "p", "b")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if calls != 1 || got.State != "READY" {
		t.Fatalf("calls=%d branch=%#v", calls, got)
	}
}

func TestLakebaseRunnerMapsNotFoundDestroy(t *testing.T) {
	runner := NewLakebaseRunner(&fakeLakebaseClient{err: errors.New("404 not found")}, false)
	result := runner.Destroy(context.Background(), lakebaseRequest("lakebase_missing"))
	if result.Status != "destroyed" {
		t.Fatalf("destroy missing = %#v", result)
	}
}

func lakebaseRequest(id string) ProvisionRequest {
	expires := time.Now().UTC().Add(time.Hour)
	return ProvisionRequest{
		Operation: ProvisionOpCreate,
		Deployment: Deployment{
			DeploymentID:       id,
			Provider:           ProviderDatabricksLakebase,
			ProviderResourceID: "pgsage-" + resourceName(id),
			LeaseExpiresAt:     &expires,
			Metadata: map[string]any{
				"provider_params": map[string]any{
					"project":       "p",
					"source_branch": "main",
				},
			},
		},
	}
}

type fakeLakebaseClient struct {
	created LakebaseBranchRequest
	err     error
}

func (f *fakeLakebaseClient) CreateBranch(
	_ context.Context,
	req LakebaseBranchRequest,
) (LakebaseBranch, error) {
	if f.err != nil {
		return LakebaseBranch{}, f.err
	}
	f.created = req
	return LakebaseBranch{
		Project: req.Project,
		Branch:  req.Branch,
		State:   "READY",
	}, nil
}

func (f *fakeLakebaseClient) GetBranch(
	_ context.Context,
	project string,
	branch string,
) (LakebaseBranch, error) {
	if f.err != nil {
		return LakebaseBranch{}, f.err
	}
	return LakebaseBranch{Project: project, Branch: branch, State: "READY"}, nil
}

func (f *fakeLakebaseClient) DeleteBranch(context.Context, string, string) error {
	return f.err
}
