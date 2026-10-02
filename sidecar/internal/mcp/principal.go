package mcp

import (
	"context"
	"strings"
)

// Principal is the authenticated caller bound to an MCP request by the
// transport (the HTTP API binds the session user; stdio binds the
// local process owner). Tool arguments can never set it.
type Principal struct {
	Actor string // stable identity, e.g. "user:42"
	Role  string // "admin", "operator" or "viewer"
}

type principalKey struct{}

// unboundActor is recorded when no principal is bound. Mutating tools
// refuse unbound requests, so it only appears on direct library use.
const unboundActor = "mcp-agent"

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the bound principal, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// ActorFromContext is the audit actor string persisted with MCP writes.
func ActorFromContext(ctx context.Context) string {
	p, ok := PrincipalFromContext(ctx)
	if !ok || strings.TrimSpace(p.Actor) == "" {
		return unboundActor
	}
	return "mcp:" + strings.TrimSpace(p.Actor)
}

// mutatingTools change policy, metadata or database state and require
// an operator or admin principal (G6-B02 / SURF-01).
var mutatingTools = map[string]bool{
	"propose_policy_change": true, "request_change": true,
	"optimize_query": true, "apply_migration": true,
	"ensure_fk_indexes": true, "declare_table_contract": true,
	"register_consumer": true, "set_maintenance_policy": true,
	"sre_propose_action": true, "sre_request_execution": true,
	"sre_downgrade_autonomy": true,
}

func canMutate(ctx context.Context) bool {
	p, ok := PrincipalFromContext(ctx)
	if !ok || strings.TrimSpace(p.Actor) == "" {
		return false
	}
	return p.Role == "admin" || p.Role == "operator"
}

// stdioPrincipal is bound to the stdio transport: only the local
// process owner, who already holds the sidecar's credentials, can
// reach it.
var stdioPrincipal = Principal{Actor: "stdio", Role: "operator"}
