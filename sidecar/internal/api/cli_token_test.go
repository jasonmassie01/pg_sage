package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// cli-01a: Bearer operator tokens on the routes the decision-loop CLI
// calls (the status reads, the approval card read and its approve). No
// concurrency test here: the middleware keeps no state between requests
// (two approvals of one change are in cli_token_db_test.go).

func TestBearerApprove_RequestMatching(t *testing.T) {
	cases := []struct {
		method, path string
		bearer, want bool
	}{
		{http.MethodGet, "/api/v1/inbox", true, true},
		{http.MethodGet, "/api/v1/actions/pending", true, true},
		{http.MethodGet, "/api/v1/whynot", true, true},
		{http.MethodGet, "/api/v1/approvals/42", true, true},
		{http.MethodPost, "/api/v1/approvals/42/approve", true, true},
		{http.MethodGet, "/api/v1/approvals/42", false, false},
		{http.MethodPost, "/api/v1/approvals/42/reject", true, false},
		{http.MethodPost, "/api/v1/approvals/42/snooze", true, false},
		{http.MethodPost, "/api/v1/inbox", true, false},
		{http.MethodGet, "/api/v1/inbox/count", true, false},
		{http.MethodGet, "/api/v1/approvals/42/approve", true, false},
		{http.MethodPost, "/api/v1/approvals//approve", true, false},
		{http.MethodPost, "/api/v1/approvals/42/approve/x", true, false},
		{http.MethodPost, "/api/v1/actions/42/approve", true, false},
		{http.MethodGet, "/api/v1/users", true, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.bearer {
			r.Header.Set("Authorization", "Bearer "+lintSecret(1))
		}
		if got := isCLITokenRequest(r); got != c.want {
			t.Errorf("%s %s bearer=%v: %v, want %v", c.method, c.path, c.bearer, got, c.want)
		}
	}
	dotted := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/42/approve", nil)
	dotted.URL.Path = "/api/v1/approvals/../users/42/approve"
	dotted.Header.Set("Authorization", "Bearer "+lintSecret(1))
	if skipsSessionAuth(dotted) {
		t.Fatal("a non-canonical path skipped session auth")
	}
	assertEscapedPathsAreSessionAuthenticated(t)
}

// Invalid input: an escaped "/" decodes to a CLI route's path but is one
// segment to the mux, so it is never taken as a CLI token request.
func assertEscapedPathsAreSessionAuthenticated(t *testing.T) {
	t.Helper()
	for _, c := range []struct{ method, raw string }{
		{http.MethodGet, "/api/v1/actions%2Fpending"},
		{http.MethodGet, "/api/v1/approvals%2F42"},
		{http.MethodPost, "/api/v1/approvals/42%2Fapprove"},
	} {
		r := httptest.NewRequest(c.method, c.raw, nil)
		r.Header.Set("Authorization", "Bearer "+lintSecret(1))
		if r.URL.RawPath == "" {
			t.Fatalf("%s: the test request has no RawPath", c.raw)
		}
		if isCLITokenRequest(r) || skipsSessionAuth(r) {
			t.Errorf("%s %s (decoded %s) was taken as a CLI token request", c.method, c.raw,
				r.URL.Path)
		}
		h, seen := cliTokenHandler(nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if seen.calls != 1 || seen.hasPrin {
			t.Errorf("%s: withCLIToken did not pass it through untouched (calls %d, "+
				"principal %v, status %d)", c.raw, seen.calls, seen.hasPrin, w.Code)
		}
	}
}

// cliSeen is what the next handler saw.
type cliSeen struct {
	calls     int
	user      *auth.User
	principal mcp.Principal
	hasPrin   bool
}

func cliTokenHandler(tokens lintTokenValidator) (http.Handler, *cliSeen) {
	seen := &cliSeen{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.calls++
		seen.user = UserFromContext(r.Context())
		seen.principal, seen.hasPrin = mcp.PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	return withCLIToken(tokens, next), seen
}

func cliTokenCall(t *testing.T, h http.Handler, method, path, secret string,
	session *auth.User) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	if session != nil {
		r = r.WithContext(context.WithValue(r.Context(), userContextKey, session))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Happy path and state: a read token reads as its owner (replacing any
// session user); an approve token approves as its owner.
func TestBearerApprove_OperatorTokenActsAsItsOwner(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): operatorGrant("rd", []string{mcptoken.ScopeRead}),
		lintSecret(2): operatorGrant("ap", []string{mcptoken.ScopeRead, mcptoken.ScopeApprove}),
	})
	h, seen := cliTokenHandler(tokens)
	code, _ := cliTokenCall(t, h, http.MethodGet, "/api/v1/inbox", lintSecret(1),
		testAdminUser())
	if code != http.StatusNoContent || seen.user == nil || seen.user.ID != 31 ||
		seen.user.Role != auth.RoleOperator || !seen.hasPrin || seen.principal.TokenID != "rd" {
		t.Fatalf("read: %d, user %+v, principal %+v", code, seen.user, seen.principal)
	}
	code, _ = cliTokenCall(t, h, http.MethodPost, "/api/v1/approvals/42/approve",
		lintSecret(2), nil)
	if code != http.StatusNoContent || seen.calls != 2 || seen.user.ID != 31 {
		t.Fatalf("approve: %d, %d calls, user %+v", code, seen.calls, seen.user)
	}
}

// Error propagation: each refusal is distinct and the next handler never
// runs; an agent token's approve is approval_reserved_for_humans.
func TestBearerApprove_RefusalsAreDistinct(t *testing.T) {
	agent := operatorGrant("ag", []string{mcptoken.ScopeRead, mcptoken.ScopeApprove})
	agent.Kind, agent.PrincipalID = mcptoken.KindAgent, "agp_1"
	ownerless := operatorGrant("ol", []string{mcptoken.ScopeRead})
	ownerless.OwnerUserID = 0
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): operatorGrant("rd", []string{mcptoken.ScopeRead}),
		lintSecret(3): agent, lintSecret(4): ownerless,
		lintSecret(5): operatorGrant("ap", []string{mcptoken.ScopeApprove}),
	})
	h, seen := cliTokenHandler(tokens)
	approve := "/api/v1/approvals/42/approve"
	cases := []struct {
		method, path, secret string
		status               int
		code, msg            string
	}{
		{http.MethodGet, "/api/v1/inbox", lintSecret(9), 401, "", "revoked"},
		{http.MethodPost, approve, lintSecret(1), 403, "scope_missing", "approve scope"},
		{http.MethodGet, "/api/v1/whynot", lintSecret(5), 403, "scope_missing", "read scope"},
		{http.MethodPost, approve, lintSecret(3), 403, "approval_reserved_for_humans", "agent"},
		{http.MethodGet, "/api/v1/inbox", lintSecret(3), 403, "agent_token", "agent"},
		{http.MethodGet, "/api/v1/approvals/42", lintSecret(3), 403,
			"approval_reserved_for_humans", "agent"},
		{http.MethodGet, "/api/v1/inbox", lintSecret(4), 403, "", "insufficient"},
	}
	for _, c := range cases {
		code, out := cliTokenCall(t, h, c.method, c.path, c.secret, nil)
		msg, _ := out["error"].(string)
		if code != c.status || (c.code != "" && out["code"] != c.code) ||
			!strings.Contains(msg, c.msg) {
			t.Errorf("%s %s: %d %v; want %d %s %q", c.method, c.path, code, out, c.status,
				c.code, c.msg)
		}
	}
	if seen.calls != 0 {
		t.Fatalf("a refused token reached the route %d times", seen.calls)
	}
	tokens.err = errors.New("control database down")
	if code, _ := cliTokenCall(t, h, http.MethodGet, "/api/v1/inbox", lintSecret(1),
		nil); code != http.StatusServiceUnavailable {
		t.Fatalf("token store down: %d, want 503", code)
	}
	nilH, nilSeen := cliTokenHandler(nil)
	if code, _ := cliTokenCall(t, nilH, http.MethodGet, "/api/v1/inbox", lintSecret(1),
		nil); code != http.StatusServiceUnavailable || nilSeen.calls != 0 {
		t.Fatalf("no token store: %d, want 503", code)
	}
}

// Boundary: a token limited to some databases must name one of them.
func TestBearerApprove_DatabaseLimitedTokenNamesItsDatabase(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): operatorGrant("lim", []string{mcptoken.ScopeRead,
			mcptoken.ScopeApprove}, "orders"),
	})
	h, seen := cliTokenHandler(tokens)
	if code, out := cliTokenCall(t, h, http.MethodGet, "/api/v1/inbox", lintSecret(1),
		nil); code != http.StatusForbidden || out["code"] != "database_required" {
		t.Fatalf("no database: %d %v, want 403 database_required", code, out)
	}
	if code, _ := cliTokenCall(t, h, http.MethodGet, "/api/v1/inbox?database=billing",
		lintSecret(1), nil); code != http.StatusNotFound {
		t.Fatalf("another database: %d, want 404", code)
	}
	if code, _ := cliTokenCall(t, h, http.MethodPost,
		"/api/v1/approvals/42/approve?database=orders", lintSecret(1), nil); code !=
		http.StatusNoContent || seen.calls != 1 {
		t.Fatalf("its database: %d, %d calls", code, seen.calls)
	}
}

// A request that is not a CLI token request passes untouched and never
// asks the token store.
func TestBearerApprove_OtherRequestsPassThrough(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{})
	h, seen := cliTokenHandler(tokens)
	op := testOperatorUser()
	if code, _ := cliTokenCall(t, h, http.MethodGet, "/api/v1/inbox", "", op); code !=
		http.StatusNoContent || seen.user != op || seen.hasPrin {
		t.Fatalf("session request: %d, user %+v", code, seen.user)
	}
	if code, _ := cliTokenCall(t, h, http.MethodPost, "/api/v1/approvals/42/reject",
		lintSecret(1), op); code != http.StatusNoContent || tokens.calls.Load() != 0 {
		t.Fatalf("reject with a Bearer header: %d, %d token checks", code, tokens.calls.Load())
	}
}
