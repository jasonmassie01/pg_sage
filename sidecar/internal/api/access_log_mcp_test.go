package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// apiauth-12 review: an MCP Bearer call has no session user; its access
// line names the token principal instead, so an agent's writes are not
// logged anonymously.

func withPrincipal(p mcp.Principal, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(r.Context(), p)))
	})
}

func serveStamped(h http.Handler) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader("{}"))
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestAccessLog_MCPPrincipalIsTheActor(t *testing.T) {
	buf := captureAccessLog(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p := mcp.Principal{Actor: "token:mcp_abc", Role: auth.RoleOperator}
	serveStamped(accessLogMiddleware(withPrincipal(p, accessPrincipalMiddleware(ok))))
	lines := accessLines(buf)
	if len(lines) != 1 || !strings.Contains(lines[0], "actor=token:mcp_abc") ||
		!strings.Contains(lines[0], "role=operator") ||
		strings.Contains(lines[0], "user_id") {
		t.Fatalf("access lines = %v", lines)
	}
}

// A session user wins over a principal bound on the same request, and an
// empty principal adds nothing. Nil: with no entry in the context (access
// log off) the principal layer only passes the request on.
func TestAccessLog_SessionUserWinsAndEmptyPrincipalIsIgnored(t *testing.T) {
	buf := captureAccessLog(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	both := fakeUserMiddleware(7, auth.RoleAdmin)(accessStampMiddleware(
		withPrincipal(mcp.Principal{Actor: "token:x"}, accessPrincipalMiddleware(ok))))
	serveStamped(accessLogMiddleware(both))
	serveStamped(accessLogMiddleware(
		withPrincipal(mcp.Principal{}, accessPrincipalMiddleware(ok))))
	lines := accessLines(buf)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "user_id=7") || strings.Contains(lines[0], "actor=") {
		t.Errorf("session line = %s", lines[0])
	}
	if strings.Contains(lines[1], "actor=") || strings.Contains(lines[1], "role=") {
		t.Errorf("empty principal line = %s", lines[1])
	}
	called := false
	bare := withPrincipal(mcp.Principal{Actor: "token:y"}, accessPrincipalMiddleware(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })))
	serveStamped(bare)
	if !called || len(accessLines(buf)) != 2 {
		t.Fatalf("no-entry request: called %v, lines %v", called, accessLines(buf))
	}
}

// DB: a real agent token's MCP call is logged with its token id, never its
// secret.
func TestAccessLog_RealMCPBearerCallNamesTheToken(t *testing.T) {
	f := newBearerFixture(t)
	tok := f.agentToken(t)
	buf := captureAccessLog(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if code := f.send(http.MethodPost, "/api/v1/mcp", "Bearer "+tok.Secret, "",
		body).Code; code != http.StatusOK {
		t.Fatalf("MCP call = %d, want 200", code)
	}
	lines := accessLines(buf)
	if len(lines) != 1 || !strings.Contains(lines[0], "actor=token:"+tok.ID) {
		t.Fatalf("access lines = %v (token %s)", lines, tok.ID)
	}
	if strings.Contains(buf.String(), tok.Secret) {
		t.Fatalf("access log leaked the token secret: %s", buf.String())
	}
}
