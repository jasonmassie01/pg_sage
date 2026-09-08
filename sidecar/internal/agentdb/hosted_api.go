package agentdb

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (c HostedHTTPClient) CreateResource(
	ctx context.Context, in HostedCreateInput,
) (HostedResource, error) {
	if c.Provider != ProviderNeon && c.Provider != ProviderSupabase {
		return HostedResource{}, ErrInvalid
	}
	if in.Mode != "branch" && in.Mode != "project" {
		return HostedResource{}, ErrInvalid
	}
	path := "/projects"
	if in.Mode == "branch" {
		path += "/" + url.PathEscape(in.Scope) + "/branches"
	}
	var body map[string]any
	var err error
	if c.Provider == ProviderNeon {
		body = neonCreateBody(in)
	} else {
		body, err = c.supabaseCreateBody(ctx, in)
	}
	if err != nil {
		return HostedResource{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return HostedResource{}, err
	}
	return c.resource(raw, in.Mode, in.Scope)
}

func neonCreateBody(in HostedCreateInput) map[string]any {
	if in.Mode == "project" {
		project := map[string]any{"name": in.Name, "org_id": in.Scope,
			"default_endpoint_settings": map[string]any{
				"autoscaling_limit_min_cu": 0.25, "autoscaling_limit_max_cu": 0.25,
			}}
		if in.Region != "" {
			project["region_id"] = in.Region
		}
		if in.Database != "" {
			project["branch"] = map[string]any{"database_name": in.Database}
		}
		return map[string]any{"project": project}
	}
	branch := map[string]any{"name": in.Name, "init_source": "parent-schema"}
	if in.Source != "" {
		branch["parent_id"] = in.Source
	}
	return map[string]any{"branch": branch,
		"endpoints": []any{map[string]any{"type": "read_write",
			"autoscaling_limit_min_cu": 0.25, "autoscaling_limit_max_cu": 0.25,
		}}}
}

func (c HostedHTTPClient) supabaseCreateBody(
	ctx context.Context, in HostedCreateInput,
) (map[string]any, error) {
	if in.Mode == "branch" {
		return map[string]any{"branch_name": in.Name, "with_data": false, "is_default": false}, nil
	}
	if in.Region == "" {
		return nil, fmt.Errorf("supabase project region is required")
	}
	if c.PasswordFunc == nil {
		return nil, fmt.Errorf("supabase database password callback is required")
	}
	password, err := c.PasswordFunc(ctx)
	if err != nil || strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("supabase database password is unavailable")
	}
	body := map[string]any{"name": in.Name, "organization_slug": in.Scope, "db_pass": password}
	if in.Region != "" {
		body["region_selection"] = map[string]any{"type": "specific", "code": in.Region}
	}
	return body, nil
}

func (c HostedHTTPClient) resourcePath(mode, scope, id string) (string, error) {
	if id == "" {
		return "", ErrInvalid
	}
	if mode == "project" {
		return "/projects/" + url.PathEscape(id), nil
	}
	if mode != "branch" || scope == "" {
		return "", ErrInvalid
	}
	if c.Provider == ProviderNeon {
		return "/projects/" + url.PathEscape(scope) + "/branches/" + url.PathEscape(id), nil
	}
	return "/branches/" + url.PathEscape(id), nil
}

func (c HostedHTTPClient) GetResource(
	ctx context.Context, mode, scope, id string,
) (HostedResource, error) {
	path, err := c.resourcePath(mode, scope, id)
	if err != nil {
		return HostedResource{}, err
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return HostedResource{}, err
	}
	resource, err := c.resource(raw, mode, scope)
	if err != nil {
		return HostedResource{}, err
	}
	if c.Provider == ProviderSupabase && mode == "branch" {
		return c.supabaseBranchHealth(ctx, resource)
	}
	if c.Provider == ProviderNeon {
		return c.neonEndpointHealth(ctx, mode, resource)
	}
	return resource, nil
}

func (c HostedHTTPClient) DeleteResource(ctx context.Context, mode, scope, id string) error {
	path, err := c.resourcePath(mode, scope, id)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, path, nil)
	return err
}

func (c HostedHTTPClient) supabaseBranchHealth(
	ctx context.Context, resource HostedResource,
) (HostedResource, error) {
	if resource.ChildProject == "" {
		return HostedResource{}, fmt.Errorf("branch project reference missing")
	}
	raw, err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(resource.ChildProject), nil)
	if err != nil {
		return HostedResource{}, err
	}
	if firstNonEmpty(stringMapValue(raw, "ref"), stringMapValue(raw, "id")) != resource.ChildProject {
		return HostedResource{}, fmt.Errorf("branch project identity mismatch")
	}
	resource.State = supabaseResourceState(stringMapValue(raw, "status"))
	resource.Endpoint = stringMapValue(mapMapValue(raw, "database"), "host")
	return resource, nil
}

func (c HostedHTTPClient) neonEndpointHealth(
	ctx context.Context, mode string, resource HostedResource,
) (HostedResource, error) {
	project := resource.Scope
	if mode == "project" {
		project = resource.ID
	}
	raw, err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(project)+"/endpoints", nil)
	if err != nil {
		return HostedResource{}, err
	}
	endpoints, _ := raw["endpoints"].([]any)
	resource.State = "provisioning"
	for _, entry := range endpoints {
		endpoint, _ := entry.(map[string]any)
		if stringMapValue(endpoint, "type") != "read_write" {
			continue
		}
		if mode == "branch" && stringMapValue(endpoint, "branch_id") != resource.ID {
			continue
		}
		resource.Endpoint = stringMapValue(endpoint, "host")
		state := stringMapValue(endpoint, "current_state")
		if state == "active" || state == "idle" {
			resource.State = "available"
		}
	}
	return resource, nil
}
