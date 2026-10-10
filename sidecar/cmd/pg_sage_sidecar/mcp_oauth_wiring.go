package main

import (
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcpauth"
)

// mcpOAuthValidator builds the OAuth resource server of the MCP endpoint
// (E2, CG-06), or nil when it is off or misconfigured: then only pg_sage's
// own MCP tokens are accepted, and the error is logged once. Identities
// resolve through sage.guard_identity_bindings on the control pool; until
// the G1 core workstream's principals exist, nothing is bound and every
// OAuth token is refused with identity_unbound.
func mcpOAuthValidator(c *config.Config, mgr *fleet.DatabaseManager,
	control *pgxpool.Pool) *mcpauth.Validator {
	if c == nil || !c.MCP.Enabled || c.MCP.Transport != "http" || !c.MCP.OAuth.Enabled {
		return nil
	}
	o := c.MCP.OAuth
	issuers := make([]mcpauth.Issuer, 0, len(o.Issuers))
	for _, iss := range o.Issuers {
		issuers = append(issuers, mcpauth.Issuer{Issuer: iss.Issuer, JWKSURI: iss.JWKSURI,
			SigningAlgs: iss.SigningAlgs})
	}
	v, err := mcpauth.New(mcpauth.Config{Resource: o.Resource, Issuers: issuers,
		TaskClaim: o.TaskClaim}, mcpauth.NewDBResolver(control), fleetDatabaseNames(mgr), nil)
	if err != nil {
		logError("mcp", "mcp.oauth is off: %v; only pg_sage MCP tokens are accepted", err)
		return nil
	}
	logInfo("mcp", "mcp.oauth: accepting access tokens from %d issuer(s) for %s",
		len(issuers), o.Resource)
	return v
}

// fleetDatabaseNames lists the monitored databases, each an OAuth resource.
func fleetDatabaseNames(mgr *fleet.DatabaseManager) func() []string {
	return func() []string {
		if mgr == nil {
			return nil
		}
		names := make([]string, 0)
		for name := range mgr.Instances() {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
}
