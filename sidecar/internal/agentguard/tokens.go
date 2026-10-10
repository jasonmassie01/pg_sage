package agentguard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// MaxTokenLifetime is the longest agent token (§8.3: expires_in_days ≤ 90).
const MaxTokenLifetime = mcptoken.MaxLifetime

// TokenRequest describes an agent token to mint for a principal
// (POST /api/v1/agents/{id}/tokens). Scopes are read and propose; an
// agent token never holds approve.
type TokenRequest struct {
	Name      string
	Scopes    []string
	Databases []string // ["*"] = every database the principal is granted
	ExpiresIn time.Duration
	CreatedBy string
}

// IssueToken mints an agent token bound to an active principal. The
// returned token carries the plaintext secret once.
func IssueToken(ctx context.Context, principals *Store, tokens *mcptoken.Store,
	principalID string, req TokenRequest) (mcptoken.Token, error) {
	if tokens == nil {
		return mcptoken.Token{}, ErrUnavailable
	}
	p, err := principals.Get(ctx, principalID)
	if err != nil {
		return mcptoken.Token{}, err
	}
	switch p.Status {
	case StatusRetired:
		return mcptoken.Token{}, fmt.Errorf("%w: principal %s", ErrRetired, p.ID)
	case StatusFrozen:
		return mcptoken.Token{}, &DeniedError{Reason: ReasonFrozen,
			Detail: "a frozen principal gets no new token", Fix: "unfreeze it first"}
	}
	tok, err := tokens.Create(ctx, mcptoken.CreateRequest{Name: req.Name,
		Kind: mcptoken.KindAgent, Scopes: req.Scopes, Databases: req.Databases,
		ExpiresIn: req.ExpiresIn, PrincipalID: p.ID, CreatedBy: req.CreatedBy})
	switch {
	case errors.Is(err, mcptoken.ErrPrincipalRequired):
		// The principal was retired between the check and the insert.
		return mcptoken.Token{}, fmt.Errorf("%w: principal %s", ErrRetired, p.ID)
	case errors.Is(err, mcptoken.ErrInvalid), errors.Is(err, mcptoken.ErrApproveForAgent):
		return mcptoken.Token{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	case err != nil:
		return mcptoken.Token{}, err
	}
	return tok, nil
}

// IdentityForGrant loads the identity of a validated token grant. ok is
// false for an operator token (a person, not an agent). A grant naming a
// principal that is gone or retired is mcptoken.ErrUnauthorized; a storage
// failure is returned as is.
func IdentityForGrant(ctx context.Context, principals *Store,
	g mcptoken.Grant) (Identity, bool, error) {
	if g.Kind != mcptoken.KindAgent {
		return Identity{}, false, nil
	}
	if g.PrincipalID == "" {
		return Identity{}, false, mcptoken.ErrUnauthorized
	}
	p, err := principals.Get(ctx, g.PrincipalID)
	if errors.Is(err, ErrNotFound) || (err == nil && p.Retired()) {
		return Identity{}, false, mcptoken.ErrUnauthorized
	}
	if err != nil {
		return Identity{}, false, err
	}
	return Identity{Principal: p, TokenID: g.TokenID, Databases: g.Databases}, true, nil
}

// Authenticate validates an MCP token secret and loads its identity: the
// whole verify path for transports other than the HTTP API.
func Authenticate(ctx context.Context, principals *Store, tokens *mcptoken.Store,
	secret string) (Identity, mcptoken.Grant, error) {
	if tokens == nil {
		return Identity{}, mcptoken.Grant{}, ErrUnavailable
	}
	g, err := tokens.Validate(ctx, secret)
	if err != nil {
		return Identity{}, mcptoken.Grant{}, err
	}
	id, _, err := IdentityForGrant(ctx, principals, g)
	if err != nil {
		return Identity{}, mcptoken.Grant{}, err
	}
	return id, g, nil
}
