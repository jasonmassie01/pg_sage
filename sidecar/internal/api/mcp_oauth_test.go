package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcpauth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

const oauthResource = "https://sage.example.com/api/v1/mcp"

// oauthIDP is an in-process issuer with a JWKS.
type oauthIDP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newOAuthIDP(t *testing.T) *oauthIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p := &oauthIDP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"}}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *oauthIDP) token(t *testing.T, sub string, aud ...string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithHeader("kid", "k"))
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"iss": p.srv.URL, "sub": sub, "aud": aud,
		"exp": time.Now().Add(time.Minute).Unix(), "scope": "pg_sage:read"})
	require.NoError(t, err)
	jws, err := signer.Sign(body)
	require.NoError(t, err)
	raw, err := jws.CompactSerialize()
	require.NoError(t, err)
	return raw
}

type fakeResolver struct{ err error }

func (f fakeResolver) PrincipalForSubject(_ context.Context, _, sub string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if sub != "agent-1" {
		return "", mcpauth.ErrNoBinding
	}
	return "agp_aaaaaaaaaaaaaaaaaaaa", nil
}

func oauthValidator(t *testing.T, p *oauthIDP, r mcpauth.Resolver) *mcpauth.Validator {
	t.Helper()
	v, err := mcpauth.New(mcpauth.Config{Resource: oauthResource,
		Issuers: []mcpauth.Issuer{{Issuer: p.srv.URL, JWKSURI: p.srv.URL + "/jwks"}}},
		r, func() []string { return []string{"orders", "billing"} }, p.srv.Client())
	require.NoError(t, err)
	return v
}

// capture records the principal and identity a request reached the MCP
// handler with.
type capture struct {
	calls     int
	principal mcp.Principal
	identity  mcpauth.Identity
}

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.calls++
	c.principal, _ = mcp.PrincipalFromContext(r.Context())
	c.identity, _ = mcpauth.IdentityFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

// oauthMux mounts the MCP routes as the router does.
func oauthMux(c *capture, v *mcpauth.Validator) *http.ServeMux {
	mux := http.NewServeMux()
	registerMCPRoutes(mux, c, nil, v)
	return mux
}

func oauthRequest(path, token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(unitListFacts))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// A token for database orders reaches the MCP handler at orders' endpoint
// as an agent principal limited to orders, with its identity attached.
func TestMCPOAuthTokenBindsAgentPrincipal(t *testing.T) {
	p := newOAuthIDP(t)
	c := &capture{}
	mux := oauthMux(c, oauthValidator(t, p, fakeResolver{}))
	tok := p.token(t, "agent-1", mcpauth.ResourceFor(oauthResource, "orders"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp/databases/orders", tok))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, c.calls)
	require.Equal(t, "principal:agp_aaaaaaaaaaaaaaaaaaaa", c.principal.Actor)
	require.Equal(t, mcp.KindAgent, c.principal.Kind)
	require.Equal(t, []string{"orders"}, c.principal.Databases)
	require.True(t, c.principal.Has(mcp.ScopeRead))
	require.False(t, c.principal.Has(mcp.ScopePropose))
	require.False(t, c.principal.Has(mcp.ScopeApprove))
	require.Equal(t, "agent-1", c.identity.Subject)
}

// G1-03 over HTTP: the token for orders is refused at billing's endpoint
// with an RFC 6750 challenge pointing at billing's metadata.
func TestMCPOAuthTokenForAnotherDatabaseIs401(t *testing.T) {
	p := newOAuthIDP(t)
	c := &capture{}
	mux := oauthMux(c, oauthValidator(t, p, fakeResolver{}))
	tok := p.token(t, "agent-1", mcpauth.ResourceFor(oauthResource, "orders"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp/databases/billing", tok))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Contains(t, w.Header().Get("WWW-Authenticate"),
		`oauth-protected-resource/api/v1/mcp/databases/billing", error="invalid_token"`)
	require.NotContains(t, w.Body.String(), tok)
	require.Zero(t, c.calls)

	foreign := p.token(t, "agent-1", "https://elsewhere.example.com")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp", foreign))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Zero(t, c.calls)
}

// Refusals are distinguishable: no token 401 with the metadata location,
// unbound identity 403, lookup failure 503, unknown database 404.
func TestMCPOAuthRefusals(t *testing.T) {
	p := newOAuthIDP(t)
	c := &capture{}
	mux := oauthMux(c, oauthValidator(t, p, fakeResolver{}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp", ""))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, `Bearer resource_metadata="https://sage.example.com/.well-known/`+
		`oauth-protected-resource/api/v1/mcp"`, w.Header().Get("WWW-Authenticate"))

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp", p.token(t, "stranger", oauthResource)))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "identity_unbound")

	broken := oauthMux(c, oauthValidator(t, p, fakeResolver{err: errors.New("db down")}))
	w = httptest.NewRecorder()
	broken.ServeHTTP(w, oauthRequest("/api/v1/mcp", p.token(t, "agent-1", oauthResource)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp/databases/nope",
		p.token(t, "agent-1", mcpauth.ResourceFor(oauthResource, "nope"))))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Zero(t, c.calls)
}

// The server-wide endpoint reaches only the databases the audience names.
func TestMCPOAuthServerWideAudience(t *testing.T) {
	p := newOAuthIDP(t)
	c := &capture{}
	mux := oauthMux(c, oauthValidator(t, p, fakeResolver{}))
	tok := p.token(t, "agent-1", oauthResource, mcpauth.ResourceFor(oauthResource, "billing"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp", tok))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, []string{"billing"}, c.principal.Databases)
	require.False(t, c.principal.MayUseDatabase("orders"))
}

// pg_sage's own MCP tokens keep working, and the per-database endpoint
// narrows them to that database.
func TestStaticTokenPrincipalNarrowedAtDatabaseEndpoint(t *testing.T) {
	c := &capture{}
	h := narrowToDatabase(c, "orders")
	req := oauthRequest("/api/v1/mcp/databases/orders", "")
	req = req.WithContext(mcp.WithPrincipal(req.Context(),
		mcp.Principal{Actor: "token:1", Kind: mcp.KindAgent}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, []string{"orders"}, c.principal.Databases)

	req = req.WithContext(mcp.WithPrincipal(req.Context(), mcp.Principal{Actor: "token:2",
		Kind: mcp.KindAgent, Databases: []string{"billing"}}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, []string{}, c.principal.Databases)
}

// The router publishes the protected-resource metadata without a session
// and mounts the per-database endpoint.
func TestRouterServesProtectedResourceMetadata(t *testing.T) {
	p := newOAuthIDP(t)
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	h := NewRouterFullRuntime(nil, cfg, nil, nil, nil, nil,
		&RuntimeDeps{MCPHandler: &capture{}, MCPOAuth: oauthValidator(t, p, fakeResolver{})},
		SessionAuthMiddleware(nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/.well-known/oauth-protected-resource/api/v1/mcp/databases/orders", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"resource":"`+oauthResource+`/databases/orders"`)

	tok := p.token(t, "agent-1", mcpauth.ResourceFor(oauthResource, "orders"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, oauthRequest("/api/v1/mcp/databases/orders", tok))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
