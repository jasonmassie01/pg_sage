package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Security of the session-or-token lint route (migration-11b): only that
// one route takes a Bearer token instead of a session, never both, and
// not in a Databricks App. No concurrency here: these are per-request
// decisions with no shared state (the concurrent test is in
// migration_lint_token_test.go).

func bearerProbe(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+lintSecret(1))
	return req
}

// The session middleware skips only a canonical POST to the lint route
// that carries a Bearer header.
func TestSkipsSessionAuthOnlyForLintBearer(t *testing.T) {
	if !skipsSessionAuth(bearerProbe(http.MethodPost, migrationLintPath)) {
		t.Fatal("a Bearer POST to the lint route must skip session auth")
	}
	plain := httptest.NewRequest(http.MethodPost, migrationLintPath, nil)
	basic := httptest.NewRequest(http.MethodPost, migrationLintPath, nil)
	basic.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	refused := map[string]*http.Request{
		"no header":         plain,
		"basic scheme":      basic,
		"GET":               bearerProbe(http.MethodGet, migrationLintPath),
		"dot segment":       bearerProbe(http.MethodPost, "/api/v1/migrations/./lint"),
		"double slash":      bearerProbe(http.MethodPost, "/api/v1/migrations//lint"),
		"trailing slash":    bearerProbe(http.MethodPost, migrationLintPath+"/"),
		"traversal":         bearerProbe(http.MethodPost, migrationLintPath+"/../../users"),
		"encoded traversal": bearerProbe(http.MethodPost, "/api/v1/migrations/lint/%2e%2e"),
	}
	for name, req := range refused {
		if skipsSessionAuth(req) {
			t.Errorf("%s: skipped session auth", name)
		}
	}
}

// A Bearer header on every other viewer-write route is still a session
// request: the session middleware refuses it without a cookie.
func TestBearerRefusedOnOtherViewerWriteRoutes(t *testing.T) {
	session := SessionAuthMiddleware(nil)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	if len(viewerWriteRoutes) < 4 {
		t.Fatalf("viewerWriteRoutes has %d entries; the table is not the registry's",
			len(viewerWriteRoutes))
	}
	for pattern := range viewerWriteRoutes {
		p := probeForPattern(pattern)
		req := bearerProbe(p.method, p.path)
		if skipsSessionAuth(req) {
			t.Errorf("%s: a Bearer header skips session auth", pattern)
		}
		rec := httptest.NewRecorder()
		session.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized ||
			!strings.Contains(rec.Body.String(), "authentication required") {
			t.Errorf("%s with Bearer and no session: %d %s", pattern, rec.Code, rec.Body)
		}
	}
	if _, listed := viewerWriteRoutes["POST "+migrationLintPath]; listed {
		t.Fatal("the lint route is still listed as viewer-write")
	}
	if viewerWriteOrTokenRoutes["POST "+migrationLintPath] == "" {
		t.Fatal("the lint route has no viewer-write-or-token reason")
	}
}

// Never both: a Bearer header with a session user uses the token only, so
// a bad token is 401 even for a signed-in viewer.
func TestMigrationLintBearerWithSessionUsesToken(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1")})
	h := lintTokenMux(lintDeps(&lintCatalog{}, 30), tokens)
	body := `{"sql":"SELECT 1","static":true}`
	rec := postLintAuth(t, h, testAdminUser(), "Bearer "+lintSecret(9), body)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "verdict") {
		t.Fatalf("bad token with a session: %d %s", rec.Code, rec.Body)
	}
	if rec := postLintAuth(t, h, testAdminUser(), "", body); rec.Code != http.StatusOK {
		t.Fatalf("session without Bearer: %d %s", rec.Code, rec.Body)
	}
}

// Through the full router: the lint route answers a Bearer request with
// the token check (no control database: 503; a malformed secret: 401),
// never with the session middleware's refusal.
func TestMigrationLintTokenThroughRouter(t *testing.T) {
	bare := buildBareRouter(t, SessionAuthMiddleware(nil))
	rec := postLintAuth(t, bare, nil, "Bearer "+lintSecret(1), `{"sql":"SELECT 1","static":true}`)
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "token validation unavailable") {
		t.Fatalf("no control database: %d %s", rec.Code, rec.Body)
	}
	standalone := buildStandaloneRouter(t, SessionAuthMiddleware(nil))
	rec = postLintAuth(t, standalone, nil, "Bearer x", `{"sql":"SELECT 1","static":true}`)
	if rec.Code != http.StatusUnauthorized ||
		strings.Contains(rec.Body.String(), "authentication required") {
		t.Fatalf("malformed secret: %d %s, want the token check's 401", rec.Code, rec.Body)
	}
	gates := map[string]routeGate{}
	for _, e := range registeredRoutes(standalone.(*http.ServeMux)) {
		gates[e.Pattern] = e.Gate
	}
	if g := gates["POST "+migrationLintPath]; g != gateViewerWriteOrToken {
		t.Fatalf("standalone lint gate = %s, want viewer-write-or-token", g)
	}
}

// Databricks App mode: the platform proxy sets its own Authorization
// header, so the lint route is session-only there: a Bearer request
// without the platform identity is 401, and with it the token is ignored.
func TestMigrationLintAppModeIsSessionOnly(t *testing.T) {
	resolve := func(context.Context, string, string) (*auth.User, error) {
		return testViewerUser(), nil
	}
	h := buildDatabricksAppRouter(t, databricksAppAuthMiddleware(resolve))
	body := `{"sql":"SELECT 1","static":true}`
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("App mode, Bearer without identity: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodPost, migrationLintPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer platform-injected")
	req.Header.Set("X-Forwarded-Email", "viewer@example.com")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"verdict"`) {
		t.Fatalf("App mode, platform identity: %d %s", out.Code, out.Body)
	}
	// One gate per pattern in every mode; App mode serves it session-only.
	for _, e := range registeredRoutes(h.(*http.ServeMux)) {
		if e.Pattern == "POST "+migrationLintPath && e.Gate != gateViewerWriteOrToken {
			t.Fatalf("App mode lint gate = %s, want viewer-write-or-token", e.Gate)
		}
	}
}

// The gate only trusts a handler built for it: any other handler behind
// gateViewerWriteOrToken is session-only, so a Bearer request is 401.
func TestRouteGate_ViewerWriteOrTokenFailsClosed(t *testing.T) {
	if gateViewerWriteOrToken.String() != "viewer-write-or-token" {
		t.Fatalf("gate name %q", gateViewerWriteOrToken)
	}
	reached := false
	h := gateViewerWriteOrToken.wrap(http.HandlerFunc(func(http.ResponseWriter,
		*http.Request) {
		reached = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerProbe(http.MethodPost, migrationLintPath))
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("bare handler, Bearer: %d reached=%v", rec.Code, reached)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withUser(httptest.NewRequest(http.MethodPost, migrationLintPath, nil),
		testViewerUser()))
	if !reached {
		t.Fatalf("bare handler, viewer session: %d, not reached", rec.Code)
	}
}

// The registry rule: viewer-write-or-token only on listed write patterns.
func TestCheckRouteGates_ViewerWriteOrToken(t *testing.T) {
	cases := []struct {
		e    routeEntry
		want bool
	}{
		{routeEntry{"POST " + migrationLintPath, gateViewerWriteOrToken}, false},
		{routeEntry{"POST /api/v1/x", gateViewerWriteOrToken}, true},
		{routeEntry{"GET " + migrationLintPath, gateViewerWriteOrToken}, true},
		{routeEntry{"POST " + migrationLintPath, gateViewerWrite}, true},
	}
	for _, tc := range cases {
		if got := len(checkRouteGates([]routeEntry{tc.e})) > 0; got != tc.want {
			t.Errorf("%+v: problem=%v, want %v", tc.e, got, tc.want)
		}
	}
	for pattern, reason := range viewerWriteOrTokenRoutes {
		if !isWriteRoute(pattern) || reason == "" {
			t.Errorf("viewerWriteOrTokenRoutes[%q] = %q: writes only, with a reason",
				pattern, reason)
		}
	}
}

// Never both, inside the handler: on the token path the session user is
// dropped and only the token principal remains.
func TestTokenOrSessionDropsSessionUser(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1")})
	var user *auth.User
	var actor string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = UserFromContext(r.Context())
		p, _ := mcp.PrincipalFromContext(r.Context())
		actor = p.Actor
		w.WriteHeader(http.StatusNoContent)
	})
	h := newTokenOrSession(tokens, probe)
	rec := postLintAuth(t, h, testAdminUser(), "Bearer "+lintSecret(1), `{}`)
	if rec.Code != http.StatusNoContent || user != nil || actor != "token:t1" {
		t.Fatalf("token with a session: %d, user %+v, actor %q", rec.Code, user, actor)
	}
	rec = postLintAuth(t, h, testAdminUser(), "", `{}`)
	if rec.Code != http.StatusNoContent || user == nil || actor != "" {
		t.Fatalf("session only: %d, user %+v, actor %q", rec.Code, user, actor)
	}
}

// A database-limited token that names no database reads the catalog of
// the database its list was checked against, never a second resolution;
// a session or an unrestricted token passes the request's database on.
func TestMigrationLintTokenReadsCheckedDatabase(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): readGrant("t1"), lintSecret(4): readGrant("t4", "alpha")})
	var mu sync.Mutex
	var read []string
	d := lintDeps(&lintCatalog{rows: 10}, 1000)
	d.querier = func(db string) (catalogread.Querier, error) {
		mu.Lock()
		defer mu.Unlock()
		read = append(read, db)
		return &lintCatalog{rows: 10}, nil
	}
	d.resolve = func(string) (string, error) { return "alpha", nil }
	h := lintTokenMux(d, tokens)
	body := `{"sql":"SELECT 1"}`
	for _, call := range []struct {
		user  *auth.User
		authz string
	}{{nil, "Bearer " + lintSecret(4)}, {testViewerUser(), ""}, {nil, "Bearer " + lintSecret(1)}} {
		if rec := postLintAuth(t, h, call.user, call.authz, body); rec.Code != http.StatusOK {
			t.Fatalf("%q: %d %s", call.authz, rec.Code, rec.Body)
		}
	}
	if want := []string{"alpha", "", ""}; !slices.Equal(read, want) {
		t.Fatalf("catalog reads for %v, want %v", read, want)
	}
}
