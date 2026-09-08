package agentdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostedDestroyReconcilesAlreadyDeletedResource(t *testing.T) {
	req := hostedRequest("neon")
	req.Deployment.ProvisioningStatus = "destroying"
	client := &hostedClientStub{err: ProviderError{Provider: "neon", Kind: ProviderErrNotFound}}
	runner := NewHostedRunner("neon", client)
	result := runner.Destroy(context.Background(), req)
	if result.Error != nil || result.Status != "destroyed" || client.deletes != 0 {
		t.Fatalf("already deleted resource did not reconcile: %+v", result)
	}
	result = runner.Status(context.Background(), req)
	if result.Error != nil || result.Status != "destroyed" {
		t.Fatalf("destroying status never completed after 404: %+v", result)
	}
}

func TestHostedSupabaseBranchHealthAndBackupEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/branches/branch-id":
			fmt.Fprint(w, `{"id":"branch-id","name":"test","parent_project_ref":"parent",
			"project_ref":"child","status":"MIGRATIONS_PASSED"}`)
		case "/projects/child":
			fmt.Fprint(w, `{"ref":"child","status":"ACTIVE_UNHEALTHY",
			"database":{"host":"child.supabase.co"}}`)
		case "/projects/child/database/backups":
			fmt.Fprint(w, `{"backups":[{"status":"COMPLETED"},{"status":"FAILED"},{"status":"PENDING"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := HostedHTTPClient{Provider: "supabase", BaseURL: server.URL,
		TokenFunc: staticToken("synthetic-token")}
	resource, err := client.GetResource(context.Background(), "branch", "parent", "branch-id")
	if err != nil || resource.State != "failed" || resource.ChildProject != "child" {
		t.Fatalf("migration status hid unhealthy database: %+v %v", resource, err)
	}
	evidence, err := client.BackupEvidence(context.Background(), "branch", "parent", "branch-id")
	if err != nil || !evidence.Available || evidence.Count != 1 {
		t.Fatalf("incorrect backup evidence: %+v %v", evidence, err)
	}
}

func TestHostedNeonEndpointSelectionKeepsBranchIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/project/branches/br-owned" {
			fmt.Fprint(w, `{"branch":{"id":"br-owned","name":"test","project_id":"project",
			"current_state":"ready"}}`)
		} else {
			fmt.Fprint(w, `{"endpoints":[{"branch_id":"br-other","type":"read_write",
			"current_state":"active","host":"other"},{"branch_id":"br-owned",
			"type":"read_write","current_state":"idle","host":"owned"}]}`)
		}
	}))
	defer server.Close()
	client := HostedHTTPClient{Provider: "neon", BaseURL: server.URL,
		TokenFunc: staticToken("synthetic-token")}
	resource, err := client.GetResource(context.Background(), "branch", "project", "br-owned")
	if err != nil || resource.Endpoint != "owned" || resource.State != "available" {
		t.Fatalf("cross-branch endpoint selected: %+v %v", resource, err)
	}
}
