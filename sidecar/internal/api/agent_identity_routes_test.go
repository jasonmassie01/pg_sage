package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-03 routes without a database: role gates, the issuer rule
// (exactly a configured mcp.oauth issuer, https or loopback http), the
// subject limits and strict decoding are all decided before the store,
// so a store with no pool shows they never reach it (it would answer 503).
// No concurrent access here: the routes are stateless; the race is in
// agent_identity_routes_db_test.go.

const identityAgent = "agp_aaaaaaaaaaaaaaaaaaaa"

func identityConfig(enabled bool, issuers ...string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.MCP.OAuth.Enabled = enabled
	for _, iss := range issuers {
		cfg.MCP.OAuth.Issuers = append(cfg.MCP.OAuth.Issuers, config.MCPOAuthIssuer{Issuer: iss})
	}
	return cfg
}

func identityMux(cfg *config.Config, role string) http.Handler {
	mux := http.NewServeMux()
	registerAgentIdentityRoutes(mux, nil, cfg)
	user := &auth.User{ID: 7, Email: role + "@example.com", Role: role}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		mux.ServeHTTP(w, r)
	})
}

func identityCall(h http.Handler, method, path, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

const identitiesPath = "/api/v1/agents/" + identityAgent + "/identities"

func bindBody(issuer, subject string) string {
	b, _ := json.Marshal(map[string]string{"issuer": issuer, "subject": subject})
	return string(b)
}

func TestIdentityRoutes_InvalidInputIs422(t *testing.T) {
	h := identityMux(identityConfig(true, "https://idp.example.com",
		"http://idp.internal.example.com", "http://127.0.0.1:9000"), auth.RoleAdmin)
	cases := []struct{ name, body string }{
		{"unknown issuer", bindBody("https://evil.example.com", "agent-1")},
		{"issuer differs by a trailing slash", bindBody("https://idp.example.com/", "a")},
		{"configured http issuer off loopback", bindBody("http://idp.internal.example.com",
			"agent-1")},
		{"empty issuer", bindBody("", "agent-1")},
		{"empty subject", bindBody("https://idp.example.com", "")},
		{"256-byte subject", bindBody("https://idp.example.com", strings.Repeat("s", 256))},
		{"control character in subject", bindBody("https://idp.example.com", "a\nb")},
		{"missing fields", `{}`},
	}
	for _, tc := range cases {
		code, body := identityCall(h, http.MethodPost, identitiesPath, tc.body)
		require.Equal(t, http.StatusUnprocessableEntity, code, "%s: %v", tc.name, body)
		require.Equal(t, "invalid_identity", body["code"], tc.name)
	}
}

func TestIdentityRoutes_MalformedBodyIs400(t *testing.T) {
	h := identityMux(identityConfig(true, "https://idp.example.com"), auth.RoleAdmin)
	for name, body := range map[string]string{
		"unknown field": `{"issuer":"https://idp.example.com","subject":"a",` +
			`"principal_id":"agp_bbbbbbbbbbbbbbbbbbbb"}`,
		"not json":      `issuer=https://idp.example.com`,
		"trailing json": bindBody("https://idp.example.com", "a") + `{}`,
		"empty body":    ``,
		"wrong type":    `{"issuer":"https://idp.example.com","subject":7}`,
	} {
		code, resp := identityCall(h, http.MethodPost, identitiesPath, body)
		require.Equal(t, http.StatusBadRequest, code, "%s: %v", name, resp)
	}
}

// The subject limit is 255 bytes: exactly 255 passes validation and
// reaches the store (here 503: no pool), 256 is refused.
func TestIdentityRoutes_SubjectBoundary(t *testing.T) {
	h := identityMux(identityConfig(true, "https://idp.example.com"), auth.RoleAdmin)
	code, body := identityCall(h, http.MethodPost, identitiesPath,
		bindBody("https://idp.example.com", strings.Repeat("s", 255)))
	require.Equal(t, http.StatusServiceUnavailable, code, "255 bytes: %v", body)
	code, _ = identityCall(h, http.MethodPost, identitiesPath,
		bindBody("https://idp.example.com", strings.Repeat("s", 256)))
	require.Equal(t, http.StatusUnprocessableEntity, code)
}

// A loopback http issuer is what mcp.oauth itself accepts (local IdPs),
// so it can be bound when configured.
func TestIdentityRoutes_LoopbackHTTPIssuerAccepted(t *testing.T) {
	h := identityMux(identityConfig(true, "http://127.0.0.1:9000"), auth.RoleAdmin)
	code, body := identityCall(h, http.MethodPost, identitiesPath,
		bindBody("http://127.0.0.1:9000", "agent-1"))
	require.Equal(t, http.StatusServiceUnavailable, code, "reaches the store: %v", body)
}

func TestIdentityRoutes_OAuthOffRefusesBind(t *testing.T) {
	h := identityMux(identityConfig(false, "https://idp.example.com"), auth.RoleAdmin)
	code, body := identityCall(h, http.MethodPost, identitiesPath,
		bindBody("https://idp.example.com", "agent-1"))
	require.Equal(t, http.StatusUnprocessableEntity, code, "%v", body)
	require.Contains(t, body["error"], "mcp.oauth")
	code, body = identityCall(h, http.MethodGet, "/api/v1/mcp/oauth/issuers", "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, false, body["enabled"])
	require.Equal(t, []any{}, body["issuers"], "no issuers while OAuth is off")
}

func TestIdentityRoutes_IssuersListsStringsOnly(t *testing.T) {
	cfg := identityConfig(true, "https://idp.example.com", "https://login.example.org")
	cfg.MCP.OAuth.Issuers[0].JWKSURI = "https://idp.example.com/secret-jwks"
	h := identityMux(cfg, auth.RoleAdmin)
	code, body := identityCall(h, http.MethodGet, "/api/v1/mcp/oauth/issuers", "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, body["enabled"])
	require.Equal(t, []any{"https://idp.example.com", "https://login.example.org"},
		body["issuers"])
	raw, _ := json.Marshal(body)
	require.NotContains(t, string(raw), "jwks")
}

func TestIdentityRoutes_OperatorAndViewerAre403(t *testing.T) {
	cfg := identityConfig(true, "https://idp.example.com")
	del := identitiesPath + "?issuer=" + url.QueryEscape("https://idp.example.com") +
		"&subject=agent-1"
	for _, role := range []string{auth.RoleOperator, auth.RoleViewer} {
		h := identityMux(cfg, role)
		for _, c := range [][3]string{
			{http.MethodPost, identitiesPath, bindBody("https://idp.example.com", "agent-1")},
			{http.MethodDelete, del, ""},
			{http.MethodGet, identitiesPath, ""},
			{http.MethodGet, "/api/v1/mcp/oauth/issuers", ""},
		} {
			code, _ := identityCall(h, c[0], c[1], c[2])
			require.Equal(t, http.StatusForbidden, code, "%s %s %s", role, c[0], c[1])
		}
	}
}

func TestIdentityRoutes_DeleteNeedsIssuerAndSubject(t *testing.T) {
	h := identityMux(identityConfig(true, "https://idp.example.com"), auth.RoleAdmin)
	for _, q := range []string{"", "?issuer=https%3A%2F%2Fidp.example.com",
		"?subject=agent-1", "?issuer=https%3A%2F%2Fidp.example.com&subject=" +
			strings.Repeat("s", 256)} {
		code, body := identityCall(h, http.MethodDelete, identitiesPath+q, "")
		require.Equal(t, http.StatusUnprocessableEntity, code, "%q: %v", q, body)
	}
	// Unbinding narrows: an issuer no longer configured can still be unbound
	// (here the store answers 503: no pool).
	code, _ := identityCall(h, http.MethodDelete, identitiesPath+
		"?issuer=https%3A%2F%2Fretired-idp.example.com&subject=agent-1", "")
	require.Equal(t, http.StatusServiceUnavailable, code)
}

func TestIdentityRoutes_InvalidAgentIDIs404(t *testing.T) {
	h := identityMux(identityConfig(true, "https://idp.example.com"), auth.RoleAdmin)
	code, _ := identityCall(h, http.MethodGet, "/api/v1/agents/nope/identities", "")
	require.Equal(t, http.StatusNotFound, code)
}

// A nil config (no mcp.oauth at all) is OAuth off, not a panic.
func TestIdentityRoutes_NilConfig(t *testing.T) {
	h := identityMux(nil, auth.RoleAdmin)
	code, body := identityCall(h, http.MethodGet, "/api/v1/mcp/oauth/issuers", "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, false, body["enabled"])
	code, _ = identityCall(h, http.MethodPost, identitiesPath,
		bindBody("https://idp.example.com", "agent-1"))
	require.Equal(t, http.StatusUnprocessableEntity, code)
}
