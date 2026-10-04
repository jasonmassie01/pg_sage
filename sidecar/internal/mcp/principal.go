package mcp

import (
	"context"
	"strings"
)

// Scope is what a principal may do through MCP: read, propose (pg_sage
// still decides through policy.Gate) or approve (a person's decision:
// confirming facts and the declarations imported as confirmed facts,
// reviewing or downgrading autonomy).
type Scope string

// Scopes.
const (
	ScopeRead    Scope = "read"
	ScopePropose Scope = "propose"
	ScopeApprove Scope = "approve"
)

// Principal kinds. A session user (Kind "") is a person; an agent token
// or the stdio client is an agent, which can never approve.
const (
	KindHuman = "human"
	KindAgent = "agent"
)

// Principal is the authenticated caller bound to an MCP request by the
// transport (the HTTP API binds the session user or the API token; stdio
// binds the local client). Tool arguments can never set it.
type Principal struct {
	Actor string // stable identity, e.g. "user:42" or "token:<id>"
	Role  string // "admin", "operator" or "viewer" (session users, token owners)
	Kind  string // "" or KindHuman for people, KindAgent for agents
	// Scopes are the granted scopes; nil derives them from Role.
	Scopes []Scope
	// Databases are the databases the principal may use; nil is all.
	Databases []string
	TokenID   string
	// Name is the token's name (the external system it belongs to).
	Name string
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

// roleScopes are the scopes a session role grants.
func roleScopes(role string) []Scope {
	switch role {
	case "admin", "operator":
		return []Scope{ScopeRead, ScopePropose, ScopeApprove}
	case "viewer":
		return []Scope{ScopeRead}
	}
	return nil
}

// Has reports whether p holds s. A blank actor holds nothing, and an agent
// never holds approve, whatever its scopes say.
func (p Principal) Has(s Scope) bool {
	if strings.TrimSpace(p.Actor) == "" || (s == ScopeApprove && p.Kind == KindAgent) {
		return false
	}
	scopes := p.Scopes
	if scopes == nil && p.Kind != KindAgent {
		scopes = roleScopes(p.Role)
	}
	for _, have := range scopes {
		if have == s {
			return true
		}
	}
	return false
}

// MayUseDatabase reports whether p may name database name.
func (p Principal) MayUseDatabase(name string) bool {
	if p.Databases == nil {
		return true
	}
	for _, allowed := range p.Databases {
		if name != "" && allowed == name {
			return true
		}
	}
	return false
}

// stdioPrincipal is bound to the stdio transport. Whatever launched the
// process over stdio is a program (a coding agent), so it can read and
// propose; approvals stay with a person in pg_sage's UI or API.
var stdioPrincipal = Principal{Actor: "stdio", Kind: KindAgent,
	Scopes: []Scope{ScopeRead, ScopePropose}}

type databaseKey struct{}

// WithDatabase returns ctx carrying the database the server resolved for
// the request; backends route by it. An empty name is not stored.
func WithDatabase(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, databaseKey{}, name)
}

// DatabaseFromContext returns the resolved database, if any.
func DatabaseFromContext(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(databaseKey{}).(string)
	return name, ok && name != ""
}
