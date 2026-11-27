package databricksauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/databricks/databricks-sdk-go/service/postgres"
)

const workspaceOrigin = "https://workspace.cloud.databricks.com"

type idRuleCase struct {
	Name, Resource string
	Valid          bool
}

func idRuleCases(t *testing.T) []idRuleCase {
	t.Helper()
	raw, err := os.ReadFile("../lakebaseid/testdata/resources.json")
	if err != nil {
		t.Fatalf("read shared verdict table: %v", err)
	}
	var cases []idRuleCase
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("parse shared verdict table: %v (%d cases)", err, len(cases))
	}
	return cases
}

// OAuth refuses a malformed SAGE_LAKEBASE_ENDPOINT before any Databricks call.
func TestOAuthEndpointVerdictsMatchSharedTable(t *testing.T) {
	for _, c := range idRuleCases(t) {
		if c.Resource == "" {
			continue // empty means "not Lakebase", which is not a verdict
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
			t.Setenv("SAGE_LAKEBASE_ENDPOINT", c.Resource)
			got, err := endpointForHost("ep.example.com")
			if (err == nil) != c.Valid {
				t.Fatalf("endpoint %q error = %v, want valid=%v", c.Resource, err, c.Valid)
			}
			if c.Valid && got != c.Resource {
				t.Fatalf("endpoint = %q, want %q", got, c.Resource)
			}
		})
	}
}

// Telemetry reads the same resource shape as OAuth; a refused resource makes
// no provider call, and an accepted one is looked up by its exact parents.
func TestTelemetryEndpointVerdictsMatchSharedTable(t *testing.T) {
	for _, c := range idRuleCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			f := &fakeTelemetryResourceAPI{fail: "endpoint"}
			_, err := resolveTelemetryResource(context.Background(), f, workspaceOrigin,
				telemetryHost, c.Resource)
			if c.Valid {
				if len(f.calls) != 1 || f.calls[0] != c.Resource {
					t.Fatalf("valid resource %q not looked up: calls %v (%v)",
						c.Resource, f.calls, err)
				}
				return
			}
			if err == nil || len(f.calls) != 0 {
				t.Fatalf("invalid resource %q reached the provider: calls %v, err %v",
					c.Resource, f.calls, err)
			}
			if !strings.Contains(err.Error(), "endpoint resource") {
				t.Fatalf("error %q does not name the endpoint resource", err)
			}
		})
	}
}

func TestTelemetryResolvesLeadingDigitEndpoint(t *testing.T) {
	const endpoint = "projects/1abc/branches/2br/endpoints/3ep"
	f := &fakeTelemetryResourceAPI{
		endpoint: &postgres.Endpoint{Name: endpoint, Parent: "projects/1abc/branches/2br",
			Uid: "ep-uid", Status: &postgres.EndpointStatus{
				Hosts: &postgres.EndpointHosts{Host: telemetryHost}}},
		branch:  &postgres.Branch{Name: "projects/1abc/branches/2br", Uid: "br-uid"},
		project: &postgres.Project{Name: "projects/1abc", Uid: "pr-uid"},
	}
	out, err := resolveTelemetryResource(context.Background(), f, workspaceOrigin,
		telemetryHost, endpoint)
	if err != nil {
		t.Fatalf("leading-digit endpoint refused: %v", err)
	}
	if out.ProjectID != "pr-uid" || out.BranchID != "br-uid" || out.EndpointID != "ep-uid" {
		t.Fatalf("resource = %+v", out)
	}
	want := []string{endpoint, "projects/1abc/branches/2br", "projects/1abc"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestTelemetryEndpointLookupErrorIsWrapped(t *testing.T) {
	f := &fakeTelemetryResourceAPI{fail: "endpoint"}
	_, err := resolveTelemetryResource(context.Background(), f, workspaceOrigin,
		telemetryHost, telemetryEndpoint)
	if err == nil || !strings.Contains(err.Error(), "verify Lakebase telemetry endpoint") ||
		errors.Unwrap(err) == nil {
		t.Fatalf("lookup error = %v, want a wrapped verify error", err)
	}
}
