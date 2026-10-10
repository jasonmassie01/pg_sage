package mcpauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// MaxTokenBytes bounds an access token; larger ones are refused unparsed.
const MaxTokenBytes = 16 << 10

// defaultRetryAfter spaces out discovery attempts against a failing issuer.
const defaultRetryAfter = 30 * time.Second

// Validator validates access tokens for this pg_sage's MCP resources.
type Validator struct {
	resource   string
	base       *url.URL
	issuers    map[string]*issuerKeys
	order      []string
	resolver   Resolver
	databases  func() []string
	taskClaim  string
	retryAfter time.Duration
	now        func() time.Time
}

// New builds a validator. databases lists the monitored databases (nil =
// none); client fetches issuer metadata and keys (nil = a 10 s client).
func New(cfg Config, resolver Resolver, databases func() []string,
	client *http.Client) (*Validator, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if resolver == nil {
		return nil, fmt.Errorf("mcp.oauth: no principal resolver")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if databases == nil {
		databases = func() []string { return nil }
	}
	base, _ := url.Parse(strings.TrimRight(cfg.Resource, "/"))
	v := &Validator{resource: base.String(), base: base, resolver: resolver,
		databases: databases, taskClaim: cfg.TaskClaim, retryAfter: defaultRetryAfter,
		now: time.Now, issuers: map[string]*issuerKeys{}}
	for _, iss := range cfg.Issuers {
		v.issuers[iss.Issuer] = &issuerKeys{cfg: iss, client: client, now: v.now}
		v.order = append(v.order, iss.Issuer)
	}
	return v, nil
}

// Resource is the server-wide resource identifier.
func (v *Validator) Resource() string { return v.resource }

// Validate checks raw for resource (the server-wide resource or one
// database's) and returns the caller's identity.
func (v *Validator) Validate(ctx context.Context, raw, resource string) (Identity, error) {
	tok, c, err := v.verify(ctx, raw)
	if err != nil {
		return Identity{}, err
	}
	if !contains(tok.Audience, resource) {
		return Identity{}, ErrAudience
	}
	scopes := grantedScopes(c)
	if len(scopes) == 0 {
		return Identity{}, ErrInsufficientScope
	}
	id, err := v.identity(tok, c)
	if err != nil {
		return Identity{}, err
	}
	id.Scopes, id.Audience, id.Expiry = scopes, tok.Audience, tok.Expiry
	id.Databases = v.reachable(resource, tok.Audience)
	principal, err := v.resolver.PrincipalForSubject(ctx, id.Issuer, id.Subject)
	switch {
	case errors.Is(err, ErrNoBinding):
		return Identity{}, err
	case err != nil:
		return Identity{}, fmt.Errorf("%w: %w", ErrResolver, err)
	}
	id.PrincipalID = principal
	return id, nil
}

// verify checks the signature, issuer and lifetime, and decodes the claims.
func (v *Validator) verify(ctx context.Context, raw string) (*oidc.IDToken, claims, error) {
	if raw == "" || len(raw) > MaxTokenBytes {
		return nil, claims{}, fmt.Errorf("%w: empty or oversized", ErrInvalidToken)
	}
	iss, err := peekIssuer(raw)
	if err != nil {
		return nil, claims{}, err
	}
	keys, ok := v.issuers[iss]
	if !ok {
		return nil, claims{}, ErrUnknownIssuer
	}
	verifier, err := keys.get(ctx, v.retryAfter)
	if err != nil {
		return nil, claims{}, err
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return nil, claims{}, fmt.Errorf("%w: claims: %w", ErrInvalidToken, err)
	}
	return tok, c, nil
}

// peekIssuer reads the unverified iss claim, only to pick the issuer whose
// keys then verify the token (and its iss again).
func peekIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: not a compact JWS", ErrInvalidToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("%w: payload encoding", ErrInvalidToken)
	}
	var head struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &head); err != nil || head.Iss == "" {
		return "", fmt.Errorf("%w: no issuer", ErrInvalidToken)
	}
	return head.Iss, nil
}

// reachable lists the databases a token reaches at resource: the one
// database of a database resource, or, at the server-wide resource, every
// monitored database whose resource the audience also names.
func (v *Validator) reachable(resource string, aud []string) []string {
	out := []string{}
	for _, db := range v.databases() {
		r := ResourceFor(v.resource, db)
		if (resource == r) || (resource == v.resource && contains(aud, r)) {
			out = append(out, db)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
