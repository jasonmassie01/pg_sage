package agentdb

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

func (c HostedHTTPClient) ValidateScope(ctx context.Context, in HostedCreateInput) error {
	if in.Mode == "project" {
		_, err := c.do(ctx, http.MethodGet, "/organizations/"+url.PathEscape(in.Scope), nil)
		return err
	}
	project, err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(in.Scope), nil)
	if err != nil {
		return err
	}
	if c.Provider == ProviderNeon {
		if stringMapValue(mapMapValue(project, "project"), "id") != in.Scope {
			return fmt.Errorf("parent project identity mismatch")
		}
		return nil
	}
	if firstNonEmpty(stringMapValue(project, "ref"), stringMapValue(project, "id")) != in.Scope {
		return fmt.Errorf("parent project identity mismatch")
	}
	org := stringMapValue(project, "organization_slug")
	if org == "" {
		return fmt.Errorf("parent project organization unavailable")
	}
	return c.validateSupabaseBranchEntitlement(ctx, org)
}

func (c HostedHTTPClient) validateSupabaseBranchEntitlement(ctx context.Context, org string) error {
	raw, err := c.do(ctx, http.MethodGet,
		"/organizations/"+url.PathEscape(org)+"/entitlements", nil)
	if err != nil {
		return err
	}
	entries, _ := raw["entitlements"].([]any)
	for _, entry := range entries {
		entitlement, _ := entry.(map[string]any)
		if stringMapValue(mapMapValue(entitlement, "feature"), "key") != "branching_limit" {
			continue
		}
		allowed, _ := entitlement["hasAccess"].(bool)
		config := mapMapValue(entitlement, "config")
		enabled, _ := config["enabled"].(bool)
		unlimited, _ := config["unlimited"].(bool)
		limit, _ := config["value"].(float64)
		if allowed && enabled && (unlimited || limit > 0) {
			return nil
		}
	}
	return providerError(ProviderSupabase, ProviderErrQuota,
		"organization does not permit database branches",
		"use project or SQL isolation within plan limits")
}
