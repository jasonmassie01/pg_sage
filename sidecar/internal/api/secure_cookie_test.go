package api

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-10: a session cookie is Secure when the request arrived over TLS,
// or when a trusted proxy says it did. X-Forwarded-Proto from any other
// peer is ignored.

// trustPeer installs a forwarded-proto trust function that trusts exactly
// the given peer IP, restoring the previous state afterwards.
func trustPeer(t *testing.T, ip string) {
	t.Helper()
	SetForwardedProtoTrust(func(r *http.Request) bool {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		return err == nil && host == ip
	})
	t.Cleanup(func() { SetForwardedProtoTrust(nil) })
}

func protoRequest(remote, proto string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://sidecar:8080/api/v1/auth/logout", nil)
	req.RemoteAddr = remote
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	return req
}

func TestSecureCookie_ForwardedProtoNeedsATrustedProxy(t *testing.T) {
	SetForwardedProtoTrust(nil)
	if secureCookie(protoRequest("203.0.113.9:4000", "https")) {
		t.Fatal("X-Forwarded-Proto with no trust function set Secure")
	}
	trustPeer(t, "10.0.0.5")
	cases := []struct {
		name, remote, proto string
		want                bool
	}{
		{"untrusted peer claims https", "203.0.113.9:4000", "https", false},
		{"trusted proxy says https", "10.0.0.5:4000", "https", true},
		{"trusted proxy says http", "10.0.0.5:4000", "http", false},
		{"trusted proxy sends no header", "10.0.0.5:4000", "", false},
		{"trusted proxy sends a list", "10.0.0.5:4000", "http,https", false},
		{"malformed peer address", "not-an-address", "https", false},
	}
	for _, tc := range cases {
		if got := secureCookie(protoRequest(tc.remote, tc.proto)); got != tc.want {
			t.Errorf("%s: secureCookie = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSecureCookie_TLSIsAlwaysSecure(t *testing.T) {
	SetForwardedProtoTrust(nil)
	req := protoRequest("203.0.113.9:4000", "http")
	req.TLS = &tls.ConnectionState{}
	if !secureCookie(req) {
		t.Fatal("TLS request without a trusted proxy did not set Secure")
	}
}

func logoutCookie(t *testing.T, req *http.Request) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	logoutHandler(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "sage_session" {
			return c
		}
	}
	t.Fatal("logout set no sage_session cookie")
	return nil
}

func TestLogoutCookie_SameSiteLaxAndTrustedSecure(t *testing.T) {
	trustPeer(t, "10.0.0.5")
	c := logoutCookie(t, protoRequest("203.0.113.9:4000", "https"))
	if c.Secure {
		t.Fatal("logout cookie Secure from an untrusted X-Forwarded-Proto")
	}
	if c.SameSite != http.SameSiteLaxMode || !c.HttpOnly || c.MaxAge >= 0 ||
		c.Value != "" || c.Path != "/" {
		t.Fatalf("logout cookie = %+v, want an expired HttpOnly Lax cookie on /", c)
	}
	c = logoutCookie(t, protoRequest("10.0.0.5:4000", "https"))
	if !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("logout cookie via trusted proxy = %+v, want Secure and Lax", c)
	}
}

func TestLoginCookie_SecureFollowsTrustedProxy(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := auth.CreateUser(ctx, pool, "cookie@test.com", "password1",
		"viewer"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	trustPeer(t, "10.0.0.5")
	h := sessionTestHandler(pool)
	for _, tc := range []struct {
		remote string
		want   bool
	}{{"203.0.113.9:4000", false}, {"10.0.0.5:4000", true}} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(`{"email":"cookie@test.com","password":"password1"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.RemoteAddr = tc.remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login from %s = %d %s", tc.remote, rec.Code, rec.Body.String())
		}
		var session *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == "sage_session" {
				session = c
			}
		}
		if session == nil || session.Value == "" {
			t.Fatalf("login from %s set no session cookie", tc.remote)
		}
		if session.Secure != tc.want || session.SameSite != http.SameSiteLaxMode {
			t.Fatalf("login from %s: Secure=%v SameSite=%v, want Secure=%v Lax",
				tc.remote, session.Secure, session.SameSite, tc.want)
		}
	}
}

func TestSecureCookie_ConcurrentTrustSwapIsSafe(t *testing.T) {
	t.Cleanup(func() { SetForwardedProtoTrust(nil) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				SetForwardedProtoTrust(func(*http.Request) bool { return true })
				SetForwardedProtoTrust(nil)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				// Untrusted-or-trusted are both valid mid-swap; a TLS
				// request must be Secure whatever the trust state.
				req := protoRequest("10.0.0.5:4000", "https")
				req.TLS = &tls.ConnectionState{}
				if !secureCookie(req) {
					t.Error("TLS request lost Secure during a trust swap")
					return
				}
			}
		}()
	}
	wg.Wait()
	if secureCookie(protoRequest("10.0.0.5:4000", "https")) {
		t.Fatal("trust left installed after the final nil swap")
	}
}

// An operator behind a TLS proxy missing from api.trusted_proxies must be
// told, once, why session cookies are not Secure.
func TestSecureCookie_UntrustedProxyWarnsOnce(t *testing.T) {
	trustPeer(t, "10.0.0.5")
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	untrustedProtoWarning = sync.Once{}
	t.Cleanup(func() { untrustedProtoWarning = sync.Once{} })

	for _, remote := range []string{"10.0.0.5:4000", "203.0.113.9:4000",
		"198.51.100.7:4000"} {
		_ = secureCookie(protoRequest(remote, "https"))
	}
	_ = secureCookie(protoRequest("203.0.113.9:4000", "http"))
	out := logs.String()
	if got := strings.Count(out, "level=WARN"); got != 1 {
		t.Fatalf("warnings = %d, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, "api.trusted_proxies") || !strings.Contains(out, "peer=203.0.113.9") {
		t.Fatalf("warning %q does not name the setting and the first untrusted peer", out)
	}
}
