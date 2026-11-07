package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcpauth"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-02 (G1-14): an OAuth-authenticated agent reaches the MCP
// handler with the policy gate's ref carrying its sponsor, its task claim
// and the RFC 8693 subject, exactly as the MCP-token path binds it.

// No concurrent access tests: serveMCPOAuth keeps no state between
// requests; each request's context is its own.

// claimsToken signs a token with exactly these claims (iss and exp added).
func (p *oauthIDP) claimsToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithHeader("kid", "k"))
	require.NoError(t, err)
	claims["iss"] = p.srv.URL
	claims["exp"] = time.Now().Add(time.Minute).Unix()
	body, err := json.Marshal(claims)
	require.NoError(t, err)
	jws, err := signer.Sign(body)
	require.NoError(t, err)
	raw, err := jws.CompactSerialize()
	require.NoError(t, err)
	return raw
}

func taskValidator(t *testing.T, p *oauthIDP) *mcpauth.Validator {
	t.Helper()
	v, err := mcpauth.New(mcpauth.Config{Resource: oauthResource, TaskClaim: "task_id",
		Issuers: []mcpauth.Issuer{{Issuer: p.srv.URL, JWKSURI: p.srv.URL + "/jwks"}}},
		fakeResolver{}, func() []string { return []string{"orders", "billing"} },
		p.srv.Client())
	require.NoError(t, err)
	return v
}

func sponsoredPrincipals(sponsor int) fakePrincipals {
	return fakePrincipals{"agp_aaaaaaaaaaaaaaaaaaaa": {
		ID: "agp_aaaaaaaaaaaaaaaaaaaa", Name: "ci-bot", Profile: "readonly-analyst",
		EnvCeiling: "stage", Status: "active", SponsorUserID: &sponsor}}
}

func serveOAuthRef(t *testing.T, ps fakePrincipals, v *mcpauth.Validator, path,
	tok string) *refCapture {
	t.Helper()
	c := &refCapture{}
	mux := http.NewServeMux()
	registerMCPRoutes(mux, c.handler(), nil, v, ps)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest(path, tok))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return c
}

func TestMCPOAuthBindsPolicyRefWithSponsorTaskAndSubject(t *testing.T) {
	p := newOAuthIDP(t)
	tok := p.claimsToken(t, map[string]any{"sub": "alice", "act": map[string]any{
		"sub": "agent-1"}, "aud": []string{mcpauth.ResourceFor(oauthResource, "orders")},
		"scope": "pg_sage:read pg_sage:propose", "task_id": "task-7"})
	c := serveOAuthRef(t, sponsoredPrincipals(41), taskValidator(t, p),
		"/api/v1/mcp/databases/orders", tok)
	require.True(t, c.bound, "an OAuth agent's call is agent-originated")
	require.Equal(t, policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", SponsorID: 41,
		TaskID: "task-7", OnBehalfOf: "alice"}, c.ref)
}

// Boundary: a principal without a sponsor and a token without a task or
// act claim give a ref with only the principal.
func TestMCPOAuthUnsponsoredPrincipalBindsBareRef(t *testing.T) {
	p := newOAuthIDP(t)
	tok := p.token(t, "agent-1", oauthResource)
	c := serveOAuthRef(t, corePrincipals, taskValidator(t, p), "/api/v1/mcp", tok)
	require.True(t, c.bound)
	require.Equal(t, policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa"}, c.ref)
}

// A refused token never reaches the handler, so nothing is bound.
func TestMCPOAuthRefusedTokenBindsNothing(t *testing.T) {
	p := newOAuthIDP(t)
	tok := p.claimsToken(t, map[string]any{"sub": "agent-1", "aud": oauthResource,
		"scope": "pg_sage:read", "task_id": 12})
	c := &refCapture{}
	mux := http.NewServeMux()
	registerMCPRoutes(mux, c.handler(), nil, taskValidator(t, p), sponsoredPrincipals(41))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp", tok))
	require.Equal(t, http.StatusUnauthorized, w.Code, "a malformed task claim fails closed")
	require.False(t, c.bound)
}

// The identity and the ref name the same principal, so the MCP server's
// bindAgentRef keeps the transport's ref.
func TestMCPOAuthRefMatchesIdentity(t *testing.T) {
	p := newOAuthIDP(t)
	tok := p.token(t, "agent-1", oauthResource)
	var id agentguard.Identity
	var ref policy.PrincipalRef
	mux := http.NewServeMux()
	registerMCPRoutes(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ = agentguard.IdentityFromContext(r.Context())
		ref, _ = policy.PrincipalRefFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), nil, taskValidator(t, p), sponsoredPrincipals(41))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, oauthRequest("/api/v1/mcp", tok))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, id.Principal.ID, ref.ID)
	require.Equal(t, *id.Principal.SponsorUserID, ref.SponsorID)
}
