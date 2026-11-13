package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// POST /api/v1/migrations/lint with an MCP API token (migration-11b): a
// read-scope token is the caller instead of a session, never both.

// fakeLintTokens is a token store: grants by secret, or err for every
// call (a storage failure). calls counts Validate calls.
type fakeLintTokens struct {
	mu     sync.Mutex
	grants map[string]mcptoken.Grant
	err    error
	calls  atomic.Int32
}

func (f *fakeLintTokens) Validate(_ context.Context, secret string) (mcptoken.Grant, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return mcptoken.Grant{}, f.err
	}
	g, ok := f.grants[secret]
	if !ok {
		return mcptoken.Grant{}, mcptoken.ErrUnauthorized
	}
	return g, nil
}

func (f *fakeLintTokens) revoke(secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grants, secret)
}

// lintSecret is a well-formed token secret: the prefix and 43 base64url
// characters.
func lintSecret(n int) string { return fmt.Sprintf("%s%043d", mcptoken.SecretPrefix, n) }

// readGrant is a read-scope operator token; no databases is every one.
func readGrant(id string, databases ...string) mcptoken.Grant {
	return mcptoken.Grant{TokenID: id, Name: "ci-" + id, Kind: mcptoken.KindOperator,
		Scopes: []string{mcptoken.ScopeRead}, Databases: databases, OwnerUserID: 2,
		OwnerRole: auth.RoleOperator}
}

func lintTokens(grants map[string]mcptoken.Grant) *fakeLintTokens {
	return &fakeLintTokens{grants: grants}
}

func lintTokenMux(d migrationLintDeps, tokens lintTokenValidator) *http.ServeMux {
	mux := http.NewServeMux()
	registerLintTokenRoute(mux, d, tokens)
	return mux
}

// postLintAuth posts body with the Authorization header authz ("" for
// none) and, when user is set, a session user.
func postLintAuth(t *testing.T, h http.Handler, user *auth.User, authz,
	body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, migrationLintPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if user != nil {
		req = withUser(req, user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Happy path: a read-scope token gets the same report a session gets for
// the same SQL, and the access log records the token principal.
func TestMigrationLintTokenMatchesSession(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1")})
	h := lintTokenMux(lintDeps(&lintCatalog{rows: 1_000_000}, 30), tokens)
	body := `{"sql":"CREATE INDEX x ON app.t (c)"}`
	viaToken := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), body)
	viaSession := postLintAuth(t, h, testViewerUser(), "", body)
	if viaToken.Code != http.StatusOK || viaSession.Code != http.StatusOK {
		t.Fatalf("token %d %s; session %d %s", viaToken.Code, viaToken.Body,
			viaSession.Code, viaSession.Body)
	}
	if viaToken.Body.String() != viaSession.Body.String() ||
		!strings.Contains(viaToken.Body.String(), `"verdict":"risky"`) {
		t.Fatalf("token body %s\nsession body %s", viaToken.Body, viaSession.Body)
	}
	if n := tokens.calls.Load(); n != 1 {
		t.Fatalf("token store consulted %d times, want once (the session call needs none)", n)
	}
	entry := &accessEntry{}
	req := httptest.NewRequest(http.MethodPost, migrationLintPath,
		strings.NewReader(`{"sql":"SELECT 1","static":true}`))
	req.Header.Set("Authorization", "bearer "+lintSecret(1))
	req = req.WithContext(context.WithValue(req.Context(), accessEntryKey{}, entry))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || entry.actor != "token:t1" ||
		entry.role != auth.RoleOperator {
		t.Fatalf("lower-case scheme: %d %s; access entry %+v", rec.Code, rec.Body, entry)
	}
}

// lintCredentialMux serves tokens t1 (every database), t2 (no read
// scope), t3 (beta only) and t4 (alpha only); "alpha" is the only
// monitored database.
func lintCredentialMux() *http.ServeMux {
	noRead := readGrant("t2")
	noRead.Scopes = []string{mcptoken.ScopePropose}
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): readGrant("t1"), lintSecret(2): noRead,
		lintSecret(3): readGrant("t3", "beta"), lintSecret(4): readGrant("t4", "alpha"),
	})
	d := lintDeps(&lintCatalog{rows: 10}, 1000)
	d.resolve = func(db string) (string, error) {
		if db == "" {
			return "alpha", nil
		}
		return db, nil
	}
	return lintTokenMux(d, tokens)
}

// Invalid input: no read scope is 403 scope_missing; a malformed or
// unknown Bearer is 401; a database outside the token's list is 404 with
// the body an unknown database gets.
func TestMigrationLintTokenInvalidCredentials(t *testing.T) {
	h := lintCredentialMux()
	cases := []struct {
		name, authz, body string
		code              int
		want              string
	}{
		{"no read scope", "Bearer " + lintSecret(2), `{"sql":"SELECT 1","static":true}`,
			403, `"code":"scope_missing"`},
		{"unknown secret", "Bearer " + lintSecret(9), `{"sql":"SELECT 1"}`, 401, "token"},
		{"empty credential", "Bearer ", `{"sql":"SELECT 1"}`, 401, "token"},
		{"garbage credential", "Bearer not a token", `{"sql":"SELECT 1"}`, 401, "token"},
		{"scheme only", "Bearer", `{"sql":"SELECT 1"}`, 401, "authentication required"},
		{"database outside list", "Bearer " + lintSecret(3),
			`{"sql":"SELECT 1","database":"alpha"}`, 404, `{"error":"database not found"}`},
		{"unknown database", "Bearer " + lintSecret(1),
			`{"sql":"SELECT 1","database":"missing"}`, 404, `{"error":"database not found"}`},
		{"static names a database outside list", "Bearer " + lintSecret(3),
			`{"sql":"SELECT 1","static":true,"database":"alpha"}`, 404, "database not found"},
		{"only database outside list", "Bearer " + lintSecret(3), `{"sql":"SELECT 1"}`,
			404, "database not found"},
		{"static without database", "Bearer " + lintSecret(3),
			`{"sql":"SELECT 1","static":true}`, 200, `"verdict"`},
		{"database inside list", "Bearer " + lintSecret(4),
			`{"sql":"SELECT 1","database":"alpha"}`, 200, `"verdict"`},
		{"only database inside list", "Bearer " + lintSecret(4), `{"sql":"SELECT 1"}`,
			200, `"verdict"`},
	}
	for _, tc := range cases {
		rec := postLintAuth(t, h, nil, tc.authz, tc.body)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want %d containing %q", tc.name, rec.Code, rec.Body,
				tc.code, tc.want)
		}
	}
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(9), `{"sql":"SELECT 1"}`)
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Fatalf("401 WWW-Authenticate = %q, want a Bearer challenge", got)
	}
}

// A token's database list without a resolver fails closed: a live review
// that names no database cannot be checked against the list.
func TestMigrationLintTokenListWithoutResolverFailsClosed(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(3): readGrant("t3", "beta")})
	h := lintTokenMux(lintDeps(&lintCatalog{rows: 10}, 1000), tokens)
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(3), `{"sql":"SELECT 1"}`)
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "verdict") {
		t.Fatalf("unresolved database with a database-limited token: %d %s", rec.Code,
			rec.Body)
	}
}

// Nil/empty: no Authorization header and no session is 401 as today, and
// a non-Bearer scheme is a session request; the token store is not asked.
func TestMigrationLintTokenNoCredentials(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{})
	h := lintTokenMux(lintDeps(&lintCatalog{}, 30), tokens)
	for _, authz := range []string{"", "Basic dXNlcjpwYXNz", "Token " + lintSecret(1)} {
		rec := postLintAuth(t, h, nil, authz, `{"sql":"SELECT 1","static":true}`)
		if rec.Code != http.StatusUnauthorized ||
			!strings.Contains(rec.Body.String(), "authentication required") {
			t.Errorf("authorization %q: %d %s, want 401 authentication required", authz,
				rec.Code, rec.Body)
		}
	}
	if n := tokens.calls.Load(); n != 0 {
		t.Fatalf("token store consulted %d times for requests without Bearer", n)
	}
}

// Error propagation: a token store that cannot answer is 503 "token
// validation unavailable" with the cause logged and the secret never
// logged; no store at all is 503 too.
func TestMigrationLintTokenStoreUnavailable(t *testing.T) {
	logs := captureSlog(t)
	tokens := &fakeLintTokens{err: fmt.Errorf("mcptoken: validating token: %w",
		errors.New("dial tcp 10.0.0.9:5432: connection refused"))}
	h := lintTokenMux(lintDeps(&lintCatalog{}, 30), tokens)
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), `{"sql":"SELECT 1","static":true}`)
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "token validation unavailable") ||
		strings.Contains(rec.Body.String(), "verdict") {
		t.Fatalf("store down: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("the cause was not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), lintSecret(1)) ||
		strings.Contains(logs.String(), mcptoken.HashSecret(lintSecret(1))) {
		t.Fatalf("the secret or its hash was logged: %q", logs.String())
	}
	h = lintTokenMux(lintDeps(&lintCatalog{}, 30), nil)
	rec = postLintAuth(t, h, nil, "Bearer "+lintSecret(1), `{"sql":"SELECT 1","static":true}`)
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "token validation unavailable") {
		t.Fatalf("no store: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(logs.String(), lintSecret(1)) {
		t.Fatalf("the secret was logged: %q", logs.String())
	}
}

// Boundary: the 31st call in a minute from one token is 429; a session
// user and another token have their own budgets.
func TestMigrationLintTokenRateLimit(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): readGrant("t1"), lintSecret(2): readGrant("t2")})
	h := lintTokenMux(lintDeps(&lintCatalog{}, 30), tokens)
	body := `{"sql":"SELECT 1","static":true}`
	for i := 0; i < 30; i++ {
		if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), body); rec.Code != 200 {
			t.Fatalf("token call %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), body)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("31st token call: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := postLintAuth(t, h, testViewerUser(), "", body); rec.Code != 200 {
		t.Fatalf("a session user was limited by a token's calls: %d %s", rec.Code, rec.Body)
	}
	if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(2), body); rec.Code != 200 {
		t.Fatalf("another token was limited: %d %s", rec.Code, rec.Body)
	}
	tokenReq := httptest.NewRequest(http.MethodPost, migrationLintPath, nil)
	tokenReq = tokenReq.WithContext(mcp.WithPrincipal(tokenReq.Context(),
		mcp.Principal{Actor: "token:t1", TokenID: "t1"}))
	if got := lintLimiterKey(tokenReq); got != "token:t1" {
		t.Fatalf("token limiter key = %q, want token:t1", got)
	}
}

// Concurrency: 50 parallel calls with one token stay within the rate
// limit (30) and the slot cap. (-race is unavailable here: no cgo.)
func TestMigrationLintTokenConcurrentCalls(t *testing.T) {
	c := &lintCatalog{rows: 10, delay: 2 * time.Millisecond}
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1")})
	h := lintTokenMux(lintDeps(c, 30), tokens)
	var ok, limited atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1),
				`{"sql":"CREATE INDEX x ON app.t (c)"}`)
			switch rec.Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 30 || limited.Load() != 20 {
		t.Fatalf("%d ok, %d limited; want 30 and 20", ok.Load(), limited.Load())
	}
	if m := c.maxFlight.Load(); m < 1 || m > migrationLintSlots {
		t.Fatalf("%d catalog reads at once, want 1..%d", m, migrationLintSlots)
	}
}

// State transitions: a token that works is 401 on the next call after it
// is revoked.
func TestMigrationLintTokenRevokedIs401(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{lintSecret(1): readGrant("t1")})
	h := lintTokenMux(lintDeps(&lintCatalog{}, 30), tokens)
	body := `{"sql":"SELECT 1","static":true}`
	if rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), body); rec.Code != 200 {
		t.Fatalf("before revoke: %d %s", rec.Code, rec.Body)
	}
	tokens.revoke(lintSecret(1))
	rec := postLintAuth(t, h, nil, "Bearer "+lintSecret(1), body)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "verdict") {
		t.Fatalf("after revoke: %d %s", rec.Code, rec.Body)
	}
}
