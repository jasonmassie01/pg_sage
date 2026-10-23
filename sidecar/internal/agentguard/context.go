package agentguard

import "context"

// Identity is the agent identity a request carries: the principal as it
// was loaded when the request was authenticated, plus the per-request
// context the transport supplies. Tool arguments can never set it.
type Identity struct {
	Principal Principal
	// TokenID is the MCP token that authenticated the request ("" for
	// stdio).
	TokenID string
	// Databases is the token's upper bound on databases (nil = all); the
	// principal's grants narrow it further.
	Databases []string
	// TaskID is a trusted runtime task claim (E2), else "". Agent-chosen
	// task ids are never put here (§6.10).
	TaskID string
	// OnBehalfOf is the human subject of a token-exchange act claim (E2).
	OnBehalfOf string
}

// MayUseDatabase reports whether the token's database bound admits name.
func (id Identity) MayUseDatabase(name string) bool {
	if id.Databases == nil {
		return name != ""
	}
	for _, d := range id.Databases {
		if d == name && name != "" {
			return true
		}
	}
	return false
}

type identityKey struct{}

// WithIdentity returns ctx carrying the request's agent identity.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFromContext returns the request's agent identity, if any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok && id.Principal.ID != ""
}

// PrincipalFromContext returns the request's principal, or ErrNoPrincipal
// when the request is not an agent's (a person, or pg_sage itself).
func PrincipalFromContext(ctx context.Context) (Principal, error) {
	id, ok := IdentityFromContext(ctx)
	if !ok {
		return Principal{}, ErrNoPrincipal
	}
	return id.Principal, nil
}
