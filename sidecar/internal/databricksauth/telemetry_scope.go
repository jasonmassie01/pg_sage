package databricksauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// TelemetryResource binds the SQL reader to provider identities verified from
// the configured direct endpoint. Display names never authorize discovery.
type TelemetryResource struct {
	Workspace  string
	ProjectID  string
	BranchID   string
	EndpointID string
}

type telemetryResourceAPI interface {
	GetProject(context.Context, postgres.GetProjectRequest) (*postgres.Project, error)
	GetBranch(context.Context, postgres.GetBranchRequest) (*postgres.Branch, error)
	GetEndpoint(context.Context, postgres.GetEndpointRequest) (*postgres.Endpoint, error)
}

func ResolveTelemetryResource(ctx context.Context, host, endpoint string) (TelemetryResource, error) {
	origin, err := WorkspaceOrigin()
	if err != nil {
		return TelemetryResource{}, err
	}
	w, err := workspace()
	if err != nil {
		return TelemetryResource{}, err
	}
	return resolveTelemetryResource(ctx, w.Postgres, origin, host, endpoint)
}

func resolveTelemetryResource(ctx context.Context, api telemetryResourceAPI, origin, host, endpoint string) (TelemetryResource, error) {
	var out TelemetryResource
	parts := strings.Split(endpoint, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "branches" || parts[4] != "endpoints" || parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return out, fmt.Errorf("lakebase telemetry requires an endpoint resource")
	}
	ep, err := api.GetEndpoint(ctx, postgres.GetEndpointRequest{Name: endpoint})
	if err != nil {
		return out, fmt.Errorf("verify Lakebase telemetry endpoint: %w", err)
	}
	if ep == nil || ep.Name != endpoint || ep.Uid == "" || ep.Status == nil || ep.Status.Hosts == nil || !allowedHost(host, []string{ep.Status.Hosts.Host}) {
		return out, fmt.Errorf("lakebase telemetry binding does not match a direct endpoint")
	}
	branchName := strings.Join(parts[:4], "/")
	projectName := strings.Join(parts[:2], "/")
	if ep.Parent != "" && ep.Parent != branchName {
		return out, fmt.Errorf("lakebase telemetry endpoint parent mismatch")
	}
	b, err := api.GetBranch(ctx, postgres.GetBranchRequest{Name: branchName})
	if err != nil {
		return out, fmt.Errorf("verify Lakebase telemetry branch: %w", err)
	}
	p, err := api.GetProject(ctx, postgres.GetProjectRequest{Name: projectName})
	if err != nil {
		return out, fmt.Errorf("verify Lakebase telemetry project: %w", err)
	}
	if b == nil || b.Name != branchName || b.Uid == "" || p == nil || p.Name != projectName || p.Uid == "" {
		return out, fmt.Errorf("lakebase telemetry resource identities unavailable")
	}
	return TelemetryResource{Workspace: origin, ProjectID: p.Uid, BranchID: b.Uid, EndpointID: ep.Uid}, nil
}
