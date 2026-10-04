package specialist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Identity is the caller: one v1.10.0 MCP token, named after the external
// system it belongs to (PagerDuty, Datadog, AWS DevOps Agent).
type Identity struct {
	TokenID   string
	Name      string
	Kind      string // "agent" or "operator"
	Scopes    []string
	Databases []string // nil = every database
	Transport string   // http, mcp, pagerduty, webhook
}

// IdentityFromGrant is the identity a validated token grants.
func IdentityFromGrant(g mcptoken.Grant, transport string) Identity {
	return Identity{TokenID: g.TokenID, Name: g.Name, Kind: string(g.Kind),
		Scopes: append([]string(nil), g.Scopes...), Databases: g.Databases,
		Transport: transport}
}

// Has reports whether the identity holds scope. Only read and propose
// exist here: approve is never held through the contract.
func (i Identity) Has(scope string) bool {
	if strings.TrimSpace(i.TokenID) == "" || (scope != ScopeRead && scope != ScopePropose) {
		return false
	}
	for _, s := range i.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// MayUse reports whether the identity may name database.
func (i Identity) MayUse(database string) bool {
	if database == "" {
		return false
	}
	if i.Databases == nil {
		return true
	}
	for _, d := range i.Databases {
		if d == database {
			return true
		}
	}
	return false
}

const maxActorBytes = 128

// Actor is the audit name: "agent:<name>:<token id>" for agent tokens,
// "token:<name>:<token id>" for operator tokens.
func (i Identity) Actor() string {
	prefix := "token:"
	if i.Kind == string(mcptoken.KindAgent) {
		prefix = "agent:"
	}
	actor := prefix + slug(i.Name) + ":" + i.TokenID
	if len(actor) > maxActorBytes {
		actor = actor[:maxActorBytes]
	}
	return actor
}

// slug is a lower-case, dash-separated, bounded form of a token name.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unnamed"
	}
	return out
}

// Authenticator validates a bearer secret into an identity. A bad,
// expired or revoked token is ErrUnauthenticated; any other error is a
// storage failure (503: a client keeps a valid token through an outage).
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) (Identity, error)
}

// TokenAuthenticator authenticates with the MCP token store.
type TokenAuthenticator struct{ store *mcptoken.Store }

// NewTokenAuthenticator wraps the MCP token store.
func NewTokenAuthenticator(store *mcptoken.Store) TokenAuthenticator {
	return TokenAuthenticator{store: store}
}

// Authenticate implements Authenticator.
func (a TokenAuthenticator) Authenticate(ctx context.Context, secret string) (Identity,
	error) {
	if a.store == nil {
		return Identity{}, ErrUnauthenticated
	}
	g, err := a.store.Validate(ctx, secret)
	if errors.Is(err, mcptoken.ErrUnauthorized) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, fmt.Errorf("%w: validating the token: %v", ErrUnavailable, err)
	}
	return IdentityFromGrant(g, "http"), nil
}
