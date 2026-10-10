package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDC login failures (CG-03). Handlers map them to 401 and audit reasons.
var (
	// ErrOAuthStateInvalid is an unknown, expired, replayed or
	// cookie-mismatched state.
	ErrOAuthStateInvalid = errors.New("oauth: invalid state")
	// ErrOIDCIDTokenMissing is a token response without an id_token.
	ErrOIDCIDTokenMissing = errors.New("oidc: token response has no id_token")
	// ErrOIDCIDTokenInvalid is an id_token whose signature, issuer,
	// audience, expiry or subject does not verify.
	ErrOIDCIDTokenInvalid = errors.New("oidc: id_token failed verification")
	// ErrOIDCNonceMismatch is an id_token whose nonce is not the one this
	// login sent (a replayed or injected token).
	ErrOIDCNonceMismatch = errors.New("oidc: id_token nonce does not match")
	// ErrOIDCSubjectMismatch is a userinfo response for another subject.
	ErrOIDCSubjectMismatch = errors.New("oidc: userinfo subject differs from id_token")
)

func (p *OAuthProvider) discoverOIDC(ctx context.Context, issuer string) error {
	issuer = strings.TrimRight(issuer, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return fmt.Errorf("oauth: building discovery request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: fetching discovery doc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: discovery returned status %d", resp.StatusCode)
	}
	var disc OIDCDiscovery
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		return fmt.Errorf("oauth: decoding discovery doc: %w", err)
	}
	if err := checkDiscovery(&disc, issuer); err != nil {
		return err
	}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), p.client),
		disc.JWKSURI)
	p.verifier = oidc.NewVerifier(disc.Issuer, keys, &oidc.Config{
		ClientID: p.cfg.ClientID, SupportedSigningAlgs: disc.SigningAlgs,
	})
	p.discovery = &disc
	return nil
}

// checkDiscovery enforces OIDC Discovery 1.0 section 4.3: the document
// must name the configured issuer, and it must publish the keys that sign
// id_tokens.
func checkDiscovery(disc *OIDCDiscovery, issuer string) error {
	if disc.AuthorizationEndpoint == "" || disc.TokenEndpoint == "" {
		return fmt.Errorf("oauth: discovery missing required endpoints")
	}
	if strings.TrimRight(disc.Issuer, "/") != issuer {
		return fmt.Errorf("oauth: discovery issuer %q does not match configured "+
			"issuer %q", disc.Issuer, issuer)
	}
	if disc.JWKSURI == "" {
		return fmt.Errorf("oauth: discovery has no jwks_uri; id_tokens cannot be verified")
	}
	return nil
}

// oidcIdentity verifies the id_token and builds the identity from its
// claims, consulting userinfo only for an email the token lacks.
func (p *OAuthProvider) oidcIdentity(
	ctx context.Context, token tokenResponse, nonce string,
) (Identity, error) {
	if token.IDToken == "" {
		return Identity{}, ErrOIDCIDTokenMissing
	}
	idToken, err := p.verifyIDToken(ctx, token.IDToken, nonce)
	if err != nil {
		return Identity{}, err
	}
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: decoding claims: %v", ErrOIDCIDTokenInvalid, err)
	}
	if claimString(claims["email"]) != "" {
		return p.identityFromClaims(idToken.Subject, claims)
	}
	id, err := p.fetchOIDCIdentity(ctx, token.AccessToken)
	if err != nil {
		return Identity{}, err
	}
	if id.Subject != idToken.Subject {
		return Identity{}, ErrOIDCSubjectMismatch
	}
	return id, nil
}

// verifyIDToken checks signature, issuer, audience and expiry (go-oidc),
// then the subject and the nonce this login sent.
func (p *OAuthProvider) verifyIDToken(
	ctx context.Context, raw, nonce string,
) (*oidc.IDToken, error) {
	if p.verifier == nil {
		return nil, fmt.Errorf("%w: discovery not performed", ErrOIDCIDTokenInvalid)
	}
	tok, err := p.verifier.Verify(oidc.ClientContext(ctx, p.client), raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOIDCIDTokenInvalid, err)
	}
	if strings.TrimSpace(tok.Subject) == "" {
		return nil, fmt.Errorf("%w: no subject", ErrOIDCIDTokenInvalid)
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(tok.Nonce), []byte(nonce)) != 1 {
		return nil, ErrOIDCNonceMismatch
	}
	return tok, nil
}

// identityFromClaims requires a verified email and reads the groups claim.
func (p *OAuthProvider) identityFromClaims(
	subject string, claims map[string]json.RawMessage,
) (Identity, error) {
	email := claimString(claims["email"])
	if email == "" {
		return Identity{}, fmt.Errorf("oauth: no email in id_token or userinfo response")
	}
	if !verifiedClaim(claims["email_verified"]) {
		return Identity{}, ErrOAuthEmailUnverified
	}
	return Identity{
		Issuer: p.issuer(), Subject: subject, Email: email, EmailVerified: true,
		Groups: groupsClaim(claims[p.groupsClaimName()]),
	}, nil
}

func (p *OAuthProvider) groupsClaimName() string {
	if p.cfg.GroupsClaim == "" {
		return "groups"
	}
	return p.cfg.GroupsClaim
}

// claimString returns a JSON string claim, or "" for any other shape.
func claimString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// groupsClaim accepts an array of strings (non-strings are skipped) or a
// single string; anything else means no groups.
func groupsClaim(raw json.RawMessage) []string {
	if s := claimString(raw); s != "" {
		return []string{s}
	}
	var items []any
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var groups []string
	for _, item := range items {
		if g, ok := item.(string); ok && g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}
