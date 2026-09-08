package agentdb

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

func (c HostedHTTPClient) resource(raw map[string]any, mode, scope string) (HostedResource, error) {
	if c.Provider == ProviderNeon {
		return neonResource(raw, mode, scope)
	}
	resource := HostedResource{Name: stringMapValue(raw, "name"), Scope: scope}
	if mode == "project" {
		resource.ID = firstNonEmpty(stringMapValue(raw, "ref"), stringMapValue(raw, "id"))
		resource.Scope = stringMapValue(raw, "organization_slug")
		resource.State = supabaseResourceState(stringMapValue(raw, "status"))
		resource.Endpoint = stringMapValue(mapMapValue(raw, "database"), "host")
		resource.ChildProject = resource.ID
	} else {
		resource.ID = stringMapValue(raw, "id")
		resource.Scope = stringMapValue(raw, "parent_project_ref")
		resource.ChildProject = stringMapValue(raw, "project_ref")
		resource.Default, _ = raw["is_default"].(bool)
		resource.State = supabaseResourceState(stringMapValue(raw, "preview_project_status"))
	}
	if resource.ID == "" || resource.Name == "" || resource.Scope == "" {
		return HostedResource{}, fmt.Errorf("provider response is missing resource identity")
	}
	return resource, nil
}

func neonResource(raw map[string]any, mode, scope string) (HostedResource, error) {
	data := mapMapValue(raw, mode)
	resource := HostedResource{ID: stringMapValue(data, "id"),
		Name: stringMapValue(data, "name"), Scope: scope, State: "provisioning"}
	if mode == "branch" {
		resource.Scope = stringMapValue(data, "project_id")
		resource.ChildProject = resource.Scope
		resource.Default, _ = data["default"].(bool)
		resource.Protected, _ = data["protected"].(bool)
	} else {
		resource.Scope = stringMapValue(data, "org_id")
		resource.ChildProject = resource.ID
	}
	endpoints, _ := raw["endpoints"].([]any)
	for _, entry := range endpoints {
		endpoint, _ := entry.(map[string]any)
		resource.Endpoint = stringMapValue(endpoint, "host")
		state := stringMapValue(endpoint, "current_state")
		if state == "active" || state == "idle" {
			resource.State = "available"
		}
	}
	if resource.ID == "" || resource.Name == "" || resource.Scope == "" {
		return HostedResource{}, fmt.Errorf("provider response is missing resource identity")
	}
	return resource, nil
}

func supabaseResourceState(state string) string {
	switch state {
	case "ACTIVE_HEALTHY":
		return "available"
	case "COMING_UP", "RESTORING", "UPGRADING", "RESTARTING", "RESIZING":
		return "provisioning"
	case "GOING_DOWN":
		return "destroying"
	case "REMOVED":
		return "destroyed"
	case "INIT_FAILED", "RESTORE_FAILED", "ACTIVE_UNHEALTHY":
		return "failed"
	default:
		return "status_unknown"
	}
}

func (c HostedHTTPClient) BackupEvidence(
	ctx context.Context, mode, scope, id string,
) (HostedBackupEvidence, error) {
	resource, err := c.GetResource(ctx, mode, scope, id)
	if err != nil {
		return HostedBackupEvidence{}, err
	}
	project := resource.ChildProject
	if c.Provider == ProviderNeon {
		raw, err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(project), nil)
		if err != nil {
			return HostedBackupEvidence{}, err
		}
		data := mapMapValue(raw, "project")
		seconds, _ := data["history_retention_seconds"].(float64)
		return HostedBackupEvidence{Available: seconds > 0, Kind: "point_in_time_restore_window",
			RetentionSeconds: int(seconds)}, nil
	}
	raw, err := c.do(ctx, http.MethodGet,
		"/projects/"+url.PathEscape(project)+"/database/backups", nil)
	if err != nil {
		return HostedBackupEvidence{}, err
	}
	backups, _ := raw["backups"].([]any)
	count := 0
	for _, entry := range backups {
		backup, _ := entry.(map[string]any)
		if stringMapValue(backup, "status") == "COMPLETED" {
			count++
		}
	}
	return HostedBackupEvidence{Available: count > 0, Kind: "managed_backup", Count: count}, nil
}
