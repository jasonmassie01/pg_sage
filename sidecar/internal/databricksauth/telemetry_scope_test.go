package databricksauth

import (
	"context"
	"errors"
	"testing"

	"github.com/databricks/databricks-sdk-go/service/postgres"
)

const telemetryEndpoint = "projects/owned/branches/test/endpoints/primary"
const telemetryHost = "ep-owned.database.us-west-2.cloud.databricks.com"

type fakeTelemetryResourceAPI struct {
	endpoint *postgres.Endpoint
	branch   *postgres.Branch
	project  *postgres.Project
	fail     string
	calls    []string
}

func telemetryAPI() *fakeTelemetryResourceAPI {
	return &fakeTelemetryResourceAPI{endpoint: &postgres.Endpoint{Name: telemetryEndpoint, Parent: "projects/owned/branches/test", Uid: "ep-internal", Status: &postgres.EndpointStatus{Hosts: &postgres.EndpointHosts{Host: telemetryHost}}}, branch: &postgres.Branch{Name: "projects/owned/branches/test", Uid: "br-internal"}, project: &postgres.Project{Name: "projects/owned", Uid: "project-internal"}}
}
func (f *fakeTelemetryResourceAPI) GetEndpoint(_ context.Context, q postgres.GetEndpointRequest) (*postgres.Endpoint, error) {
	f.calls = append(f.calls, q.Name)
	if f.fail == "endpoint" {
		return nil, errors.New("denied")
	}
	return f.endpoint, nil
}
func (f *fakeTelemetryResourceAPI) GetBranch(_ context.Context, q postgres.GetBranchRequest) (*postgres.Branch, error) {
	f.calls = append(f.calls, q.Name)
	if f.fail == "branch" {
		return nil, errors.New("denied")
	}
	return f.branch, nil
}
func (f *fakeTelemetryResourceAPI) GetProject(_ context.Context, q postgres.GetProjectRequest) (*postgres.Project, error) {
	f.calls = append(f.calls, q.Name)
	if f.fail == "project" {
		return nil, errors.New("denied")
	}
	return f.project, nil
}

func TestTelemetryResourceUsesVerifiedInternalIDsAndDirectHost(t *testing.T) {
	f := telemetryAPI()
	out, err := resolveTelemetryResource(context.Background(), f, "https://workspace.cloud.databricks.com", telemetryHost, telemetryEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if out.ProjectID != "project-internal" || out.BranchID != "br-internal" || out.EndpointID != "ep-internal" || out.Workspace != "https://workspace.cloud.databricks.com" {
		t.Fatalf("bad binding %+v", out)
	}
	if len(f.calls) != 3 || f.calls[0] != telemetryEndpoint || f.calls[1] != "projects/owned/branches/test" || f.calls[2] != "projects/owned" {
		t.Fatalf("unscoped discovery %+v", f.calls)
	}
	f = telemetryAPI()
	if _, err = resolveTelemetryResource(context.Background(), f, "https://workspace.cloud.databricks.com", "EP-OWNED.DATABASE.US-WEST-2.CLOUD.DATABRICKS.COM.", telemetryEndpoint); err != nil {
		t.Fatal("canonical equivalent hostname rejected", err)
	}
}

func TestTelemetryResourceRejectsHostileHostsResourcesAndMissingIdentity(t *testing.T) {
	for _, host := range []string{telemetryHost + ".attacker.invalid", "attacker.invalid", "https://" + telemetryHost, telemetryHost + ":443", "user@" + telemetryHost, "ep-owned-pooler.database.us-west-2.cloud.databricks.com"} {
		t.Run(host, func(t *testing.T) {
			if _, err := resolveTelemetryResource(context.Background(), telemetryAPI(), "https://workspace.cloud.databricks.com", host, telemetryEndpoint); err == nil {
				t.Fatal("accepted unverified host")
			}
		})
	}
	for _, endpoint := range []string{"", "projects/owned", "projects/owned/branches/test/endpoints/", "projects/owned/branches/test/endpoints/primary/extra", "projects/other/branches/test/endpoints/primary"} {
		if _, err := resolveTelemetryResource(context.Background(), telemetryAPI(), "https://workspace.cloud.databricks.com", telemetryHost, endpoint); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	cases := map[string]func(*fakeTelemetryResourceAPI){
		"endpoint-missing": func(f *fakeTelemetryResourceAPI) { f.endpoint = nil }, "endpoint-id": func(f *fakeTelemetryResourceAPI) { f.endpoint.Uid = "" }, "status": func(f *fakeTelemetryResourceAPI) { f.endpoint.Status = nil }, "hosts": func(f *fakeTelemetryResourceAPI) { f.endpoint.Status.Hosts = nil },
		"parent": func(f *fakeTelemetryResourceAPI) { f.endpoint.Parent = "projects/other/branches/test" }, "branch-id": func(f *fakeTelemetryResourceAPI) { f.branch.Uid = "" }, "branch-name": func(f *fakeTelemetryResourceAPI) { f.branch.Name = "projects/other/branches/test" }, "branch-missing": func(f *fakeTelemetryResourceAPI) { f.branch = nil },
		"project-id": func(f *fakeTelemetryResourceAPI) { f.project.Uid = "" }, "project-name": func(f *fakeTelemetryResourceAPI) { f.project.Name = "projects/other" }, "project-missing": func(f *fakeTelemetryResourceAPI) { f.project = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := telemetryAPI()
			mutate(f)
			if _, err := resolveTelemetryResource(context.Background(), f, "https://workspace.cloud.databricks.com", telemetryHost, telemetryEndpoint); err == nil {
				t.Fatal("accepted unavailable/mismatched resource identity")
			}
		})
	}
	for _, operation := range []string{"endpoint", "branch", "project"} {
		f := telemetryAPI()
		f.fail = operation
		if _, err := resolveTelemetryResource(context.Background(), f, "https://workspace.cloud.databricks.com", telemetryHost, telemetryEndpoint); err == nil {
			t.Fatalf("ignored %s failure", operation)
		}
	}
}
