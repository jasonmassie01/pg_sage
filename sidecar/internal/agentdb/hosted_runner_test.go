package agentdb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type hostedClientStub struct {
	resource               HostedResource
	err                    error
	creates, deletes, gets int
	input                  HostedCreateInput
	backup                 HostedBackupEvidence
}

func (s *hostedClientStub) CreateResource(
	_ context.Context, in HostedCreateInput,
) (HostedResource, error) {
	s.creates++
	s.input = in
	return s.resource, s.err
}
func (s *hostedClientStub) GetResource(_ context.Context, _, _, _ string) (HostedResource, error) {
	s.gets++
	return s.resource, s.err
}
func (s *hostedClientStub) DeleteResource(_ context.Context, _, _, _ string) error {
	s.deletes++
	return s.err
}
func (s *hostedClientStub) BackupEvidence(
	_ context.Context, _, _, _ string,
) (HostedBackupEvidence, error) {
	return s.backup, s.err
}

func hostedRequest(provider string) ProvisionRequest {
	return ProvisionRequest{Deployment: Deployment{
		DeploymentID: "test-deployment", Provider: provider, ProvisioningLevel: LevelInstance,
		ProviderResourceID: "recorded-id", SecretRef: "env:TEST_DATABASE_URL",
		Metadata: map[string]any{"provider_params": map[string]any{
			"mode": "branch", "project": "owned-project", "source_branch": "source-id",
			"region": "us-east-1",
		}},
	}}
}

func TestHostedRunnerCreateAndStatus(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		t.Run(provider, func(t *testing.T) {
			req := hostedRequest(provider)
			name, err := ProviderResourceName(provider, req.Deployment.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			client := &hostedClientStub{resource: HostedResource{
				ID: "recorded-id", Name: name, Scope: "owned-project", State: "available",
			}}
			runner := NewHostedRunner(provider, client)
			if got := runner.Preflight(context.Background(), req); got.Status != "preflight_passed" {
				t.Fatalf("preflight: %+v", got)
			}
			got := runner.Create(context.Background(), req)
			if got.Error != nil || got.Status != "available" || got.ProviderResourceID != "recorded-id" {
				t.Fatalf("create: %+v", got)
			}
			if client.creates != 1 || client.input.Name != name || client.input.Scope != "owned-project" {
				t.Fatalf("wrong create input/calls: %+v", client)
			}
			got = runner.Status(context.Background(), req)
			if got.Status != "available" || got.SecretRef != req.Deployment.SecretRef {
				t.Fatalf("status: %+v", got)
			}
		})
	}
}

func TestHostedRunnerRefusesUnownedDestroy(t *testing.T) {
	cases := []string{"wrong name", "wrong parent", "default", "protected", "missing recorded id"}
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			req := hostedRequest("neon")
			name, _ := ProviderResourceName("neon", req.Deployment.DeploymentID)
			resource := HostedResource{ID: "recorded-id", Name: name, Scope: "owned-project"}
			switch scenario {
			case "wrong name":
				resource.Name = "someone-elses-branch"
			case "wrong parent":
				resource.Scope = "other-project"
			case "default":
				resource.Default = true
			case "protected":
				resource.Protected = true
			case "missing recorded id":
				req.Deployment.ProviderResourceID = ""
			}
			client := &hostedClientStub{resource: resource}
			got := NewHostedRunner("neon", client).Destroy(context.Background(), req)
			if got.Error == nil || client.deletes != 0 {
				t.Fatalf("unsafe destroy: result=%+v calls=%d", got, client.deletes)
			}
		})
	}
}

func TestHostedRunnerDestroyRecordedOwnedResource(t *testing.T) {
	req := hostedRequest("supabase")
	name, _ := ProviderResourceName("supabase", req.Deployment.DeploymentID)
	client := &hostedClientStub{resource: HostedResource{
		ID: "recorded-id", Name: name, Scope: "owned-project",
	}}
	got := NewHostedRunner("supabase", client).Destroy(context.Background(), req)
	if got.Error != nil || got.Status != "destroying" || client.gets != 1 || client.deletes != 1 {
		t.Fatalf("destroy result=%+v calls=%+v", got, client)
	}
}

func TestHostedRunnerInvalidAndUncertainCreate(t *testing.T) {
	req := hostedRequest("neon")
	got := NewHostedRunner("neon", nil).Preflight(context.Background(), req)
	if got.Error == nil || got.Status != "preflight_failed" {
		t.Fatalf("nil client: %+v", got)
	}
	client := &hostedClientStub{err: context.DeadlineExceeded}
	got = NewHostedRunner("neon", client).Create(context.Background(), req)
	if got.Status != "status_unknown" || got.Error == nil || client.creates != 1 {
		t.Fatalf("uncertain create must not retry: %+v calls=%d", got, client.creates)
	}
	if !errors.Is(got.Error, context.DeadlineExceeded) {
		t.Fatal("context cause was lost")
	}
	req.Deployment.DeploymentID = ""
	got = NewHostedRunner("neon", client).Create(context.Background(), req)
	if got.Error == nil || client.creates != 1 {
		t.Fatal("empty deployment created resource")
	}
}

func TestHostedBackupEvidenceDoesNotClaimRestore(t *testing.T) {
	req := hostedRequest("supabase")
	name, _ := ProviderResourceName("supabase", req.Deployment.DeploymentID)
	client := &hostedClientStub{resource: HostedResource{ID: "recorded-id", Name: name,
		Scope: "owned-project"}}
	runner := NewHostedRunner("supabase", client)
	if got := runner.BackupCheck(context.Background(), req); got.Status == "verified" {
		t.Fatalf("missing backup falsely verified: %+v", got)
	}
	client.backup = HostedBackupEvidence{Available: true, Kind: "managed_backup", Count: 1}
	got := runner.BackupCheck(context.Background(), req)
	if got.Status != "verified" || got.Detail["restore_verified"] != false ||
		got.Detail["backup_count"] != 1 {
		t.Fatalf("backup evidence: %+v", got)
	}
}

func TestHostedResourceNamesAvoidTruncationCollision(t *testing.T) {
	a, errA := ProviderResourceName("neon", strings.Repeat("a", 80)+"x")
	b, errB := ProviderResourceName("neon", strings.Repeat("a", 80)+"y")
	if errA != nil || errB != nil || a == b || len(a) > 63 || len(b) > 63 {
		t.Fatalf("invalid names/collision: %q %q %v %v", a, b, errA, errB)
	}
}
