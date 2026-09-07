package agentdb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCloudSQLRunnerCreateStatusDestroy(t *testing.T) {
	client := &fakeCloudSQLClient{}
	runner := NewCloudSQLRunner(client, "proj", "us-central1")
	req := cloudSQLRequest("cloudsql_runner_1")
	preflight := runner.Preflight(context.Background(), req)
	if preflight.Status != "preflight_passed" ||
		preflight.ProviderResourceID != "pgsage-cloudsql-runner-1" {
		t.Fatalf("preflight = %#v", preflight)
	}
	created := runner.Create(context.Background(), req)
	if created.Status != "available" ||
		created.ProviderResourceID != "pgsage-cloudsql-runner-1" {
		t.Fatalf("created = %#v", created)
	}
	if !client.created.BackupEnabled || !client.created.RequireSSL {
		t.Fatalf("missing backup/ssl safeguards: %#v", client.created)
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

func TestCloudSQLRunnerRejectsUnsafeNetwork(t *testing.T) {
	req := cloudSQLRequest("cloudsql_network")
	params := req.Deployment.Metadata["provider_params"].(map[string]any)
	params["ipv4_enabled"] = true
	if got := NewCloudSQLRunner(&fakeCloudSQLClient{}, "proj", "us-central1").
		Preflight(context.Background(), req); got.Error == nil {
		t.Fatal("expected public IP policy rejection")
	}
	params["ipv4_enabled"] = false
	params["authorized_networks"] = []any{"0.0.0.0/0"}
	req.Policy.AllowPublicIP = true
	if got := NewCloudSQLRunner(&fakeCloudSQLClient{}, "proj", "us-central1").
		Preflight(context.Background(), req); got.Error == nil {
		t.Fatal("expected broad authorized network rejection")
	}
}

func TestCloudSQLRunnerMapsErrors(t *testing.T) {
	runner := NewCloudSQLRunner(
		&fakeCloudSQLClient{err: errors.New("quota exceeded")},
		"proj", "us-central1",
	)
	result := runner.Create(context.Background(), cloudSQLRequest("cloudsql_err"))
	if result.Error == nil {
		t.Fatal("expected error")
	}
	if mapped := publicProviderError(result.Error); !errors.Is(mapped, ErrInvalid) {
		t.Fatalf("mapped = %v", mapped)
	}
	runner = NewCloudSQLRunner(
		&fakeCloudSQLClient{err: errors.New("404 not found")},
		"proj", "us-central1",
	)
	result = runner.Destroy(context.Background(), cloudSQLRequest("cloudsql_missing"))
	if result.Status != "destroyed" {
		t.Fatalf("destroy missing = %#v", result)
	}
}

func TestCloudSQLHTTPClientCreateStatusDestroy(t *testing.T) {
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("auth header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/sql/v1beta4/projects/proj/instances":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			settings := body["settings"].(map[string]any)
			if body["name"] != "pgsage-live" ||
				settings["tier"] != "db-f1-micro" ||
				settings["edition"] != "ENTERPRISE" {
				t.Fatalf("unexpected create body: %#v", body)
			}
			backup := settings["backupConfiguration"].(map[string]any)
			if backup["enabled"] != true || backup["pointInTimeRecoveryEnabled"] != true {
				t.Fatalf("backup config = %#v", backup)
			}
			ipConfig := settings["ipConfiguration"].(map[string]any)
			if ipConfig["ipv4Enabled"] != false {
				t.Fatalf("ip config = %#v", ipConfig)
			}
			sawCreate = true
			_, _ = w.Write([]byte(`{"name":"operations/create-1"}`))
		case r.Method == http.MethodGet &&
			r.URL.Path == "/sql/v1beta4/projects/proj/instances/pgsage-live":
			_, _ = w.Write([]byte(`{
				"name":"pgsage-live",
				"state":"RUNNABLE",
				"connectionName":"proj:us-central1:pgsage-live"
			}`))
		case r.Method == http.MethodDelete &&
			r.URL.Path == "/sql/v1beta4/projects/proj/instances/pgsage-live":
			_, _ = w.Write([]byte(`{"name":"operations/delete-1"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := CloudSQLHTTPClient{
		BaseURL: server.URL,
		TokenFunc: func(context.Context) (string, error) {
			return "test-token", nil
		},
		HTTPClient: server.Client(),
	}
	input := CloudSQLCreateInput{
		Project:         "proj",
		Region:          "us-central1",
		Name:            "pgsage-live",
		DatabaseVersion: "POSTGRES_16",
		Tier:            "db-f1-micro",
		Edition:         "ENTERPRISE",
		StorageGB:       20,
		BackupEnabled:   true,
		RequireSSL:      true,
		Labels:          map[string]string{"app": "pg-sage"},
	}
	created, err := client.CreateInstance(context.Background(), input)
	if err != nil {
		t.Fatalf("create err = %v", err)
	}
	if !sawCreate || created.Name != "pgsage-live" || created.State != "PENDING_CREATE" {
		t.Fatalf("created = %#v sawCreate=%v", created, sawCreate)
	}
	got, err := client.GetInstance(context.Background(), "proj", "pgsage-live")
	if err != nil {
		t.Fatalf("get err = %v", err)
	}
	if got.State != "RUNNABLE" || got.ConnectionName != "proj:us-central1:pgsage-live" {
		t.Fatalf("got = %#v", got)
	}
	if err := client.DeleteInstance(context.Background(), "proj", "pgsage-live"); err != nil {
		t.Fatalf("delete err = %v", err)
	}
}

func cloudSQLRequest(id string) ProvisionRequest {
	expires := time.Now().UTC().Add(time.Hour)
	return ProvisionRequest{
		Operation: ProvisionOpCreate,
		Policy:    LiveProvisionPolicy{AllowedRegions: []string{"us-central1"}},
		Deployment: Deployment{
			DeploymentID:       id,
			Provider:           ProviderGCPCloudSQL,
			ProviderResourceID: "pgsage-" + resourceName(id),
			LeaseExpiresAt:     &expires,
			Metadata: map[string]any{
				"disposable": true,
				"provider_params": map[string]any{
					"project":      "proj",
					"region":       "us-central1",
					"tier":         "db-f1-micro",
					"edition":      "ENTERPRISE",
					"storage_size": 20,
					"ipv4_enabled": false,
				},
			},
		},
	}
}

type fakeCloudSQLClient struct {
	created CloudSQLCreateInput
	err     error
}

func (f *fakeCloudSQLClient) CreateInstance(
	_ context.Context,
	input CloudSQLCreateInput,
) (CloudSQLInstance, error) {
	if f.err != nil {
		return CloudSQLInstance{}, f.err
	}
	f.created = input
	return CloudSQLInstance{
		Name:           input.Name,
		State:          "RUNNABLE",
		ConnectionName: input.Project + ":" + input.Region + ":" + input.Name,
	}, nil
}

func (f *fakeCloudSQLClient) GetInstance(
	_ context.Context,
	_ string,
	name string,
) (CloudSQLInstance, error) {
	if f.err != nil {
		return CloudSQLInstance{}, f.err
	}
	return CloudSQLInstance{Name: name, State: "RUNNABLE"}, nil
}

func (f *fakeCloudSQLClient) DeleteInstance(context.Context, string, string) error {
	return f.err
}
