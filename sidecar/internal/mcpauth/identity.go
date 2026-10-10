package mcpauth

import (
	"context"
	"errors"
	"time"
)

// Validation outcomes. The HTTP layer maps them: invalid token, audience
// and unknown issuer to 401 invalid_token; insufficient scope to 403
// insufficient_scope; no binding to 403; an unreachable issuer or resolver
// to 503 (a client must not discard a token that may be valid).
var (
	ErrInvalidToken      = errors.New("mcpauth: invalid access token")
	ErrAudience          = errors.New("mcpauth: token audience is not this resource")
	ErrUnknownIssuer     = errors.New("mcpauth: token issuer is not allowed")
	ErrIssuerUnavailable = errors.New("mcpauth: issuer keys unavailable")
	ErrInsufficientScope = errors.New("mcpauth: token lacks the pg_sage:read scope")
	ErrNoBinding         = errors.New("mcpauth: no principal is bound to this identity")
	ErrResolver          = errors.New("mcpauth: principal lookup failed")
)

// Scope values in tokens, and what they grant.
const (
	ScopeRead    = "pg_sage:read"
	ScopePropose = "pg_sage:propose"
)

// Identity is a validated token's caller.
type Identity struct {
	Issuer      string
	Subject     string // the acting party: act.sub when delegated, else sub
	PrincipalID string
	// OnBehalfOf is whom a delegated (RFC 8693) token acts for: its sub.
	OnBehalfOf string
	// DelegationChain lists prior actors of a nested act claim, outermost
	// first (excluding Subject).
	DelegationChain []string
	TaskID          string
	// Databases are the databases the token reaches (never nil: empty
	// means none).
	Databases []string
	Scopes    []string // "read", "propose"
	Audience  []string
	Expiry    time.Time
}

// Resolver maps an external identity to a principal id. It returns
// ErrNoBinding when none is bound; any other error is a lookup failure.
// DBResolver reads the E2 bindings table (sage.guard_identity_bindings);
// the HTTP layer then loads the principal from core's agentguard store.
type Resolver interface {
	PrincipalForSubject(ctx context.Context, issuer, subject string) (string, error)
}
