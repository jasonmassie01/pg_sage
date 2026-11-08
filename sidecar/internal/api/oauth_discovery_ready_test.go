package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/fakeidp"
)

// apiauth-07: while OIDC discovery has not succeeded, SSO answers
// "enabled but not ready" and 503 sso_not_ready instead of looking
// unconfigured (404), and it recovers without a restart.

func discoveryOAuthConfig(idp *fakeidp.IdP) config.OAuthConfig {
	return config.OAuthConfig{Enabled: true, Provider: "oidc", ClientID: idp.ClientID,
		ClientSecret: "stub-secret", RedirectURL: "https://sage.test/cb",
		IssuerURL: idp.Issuer(), DefaultRole: auth.RoleViewer}
}

func serveOAuth(h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, target, strings.NewReader(body)))
	return w
}

func decodeAny(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q is not JSON: %v", w.Body.String(), err)
	}
	return m
}

func assertSSONotReady(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s: status = %d, want 503 (%s)", what, w.Code, w.Body)
	}
	m := decodeAny(t, w)
	if m["code"] != "sso_not_ready" || m["reason"] != auth.ReasonIdPUnreachable {
		t.Fatalf("%s: body = %v, want code sso_not_ready and the reason", what, m)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatalf("%s: 503 without Retry-After", what)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookieName && c.MaxAge > 0 {
			t.Fatalf("%s: set an oauth_state cookie while not ready", what)
		}
	}
}

func TestOAuthNotReady_503ThenRecoversWithoutRestart(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.FailDiscovery(1, http.StatusServiceUnavailable)
	cfg := discoveryOAuthConfig(idp)
	provider := auth.NewOAuthProvider(&cfg)
	if err := provider.Discover(context.Background()); err == nil {
		t.Fatal("discovery against a 503 succeeded")
	}

	conf := decodeAny(t, serveOAuth(oauthConfigHandler(provider, "oidc"),
		"GET", "/api/v1/auth/oauth/config", ""))
	if conf["enabled"] != true || conf["ready"] != false ||
		conf["reason"] != auth.ReasonIdPUnreachable {
		t.Fatalf("config while not ready = %v, want enabled, not ready, reason", conf)
	}
	authorize := oauthAuthorizeRouter(provider, nil)
	assertSSONotReady(t, "authorize", serveOAuth(authorize, "GET",
		"/api/v1/auth/oauth/authorize", ""))
	// A link needs a session first: without one the answer stays 401 and
	// says nothing about SSO (signed-in case: the DB-backed test below).
	if w := serveOAuth(authorize, "GET", "/api/v1/auth/oauth/authorize?intent=link",
		""); w.Code != http.StatusUnauthorized {
		t.Fatalf("authorize link without a session = %d %s, want 401", w.Code, w.Body)
	}
	assertSSONotReady(t, "callback", serveOAuth(
		oauthCallbackHandler(provider, nil, &cfg), "GET",
		"/api/v1/auth/oauth/callback?code=c&state=s", ""))
	// nil pool: a grant must not be redeemed (used up) while SSO cannot finish.
	assertSSONotReady(t, "link grant", serveOAuth(oauthLinkGrantHandler(provider, nil),
		"POST", oauthLinkGrantPath, `{"grant":"g"}`))

	if err := provider.Discover(context.Background()); err != nil {
		t.Fatalf("discovery after the IdP recovered: %v", err)
	}
	conf = decodeAny(t, serveOAuth(oauthConfigHandler(provider, "oidc"),
		"GET", "/api/v1/auth/oauth/config", ""))
	if conf["enabled"] != true || conf["ready"] != true {
		t.Fatalf("config after recovery = %v, want enabled and ready", conf)
	}
	if _, has := conf["reason"]; has {
		t.Fatalf("config after recovery still carries a reason: %v", conf)
	}
	w := serveOAuth(authorize, "GET", "/api/v1/auth/oauth/authorize", "")
	got := decodeAny(t, w)
	url, _ := got["url"].(string)
	if w.Code != http.StatusOK || !strings.HasPrefix(url, idp.Issuer()+"/authorize?") {
		t.Fatalf("authorize after recovery = %d %v, want the IdP authorize URL", w.Code, got)
	}
}

func TestOAuthDisabled_AnswersUnchanged(t *testing.T) {
	conf := decodeAny(t, serveOAuth(oauthConfigHandler(nil, "oidc"),
		"GET", "/api/v1/auth/oauth/config", ""))
	if len(conf) != 2 || conf["enabled"] != false || conf["provider"] != "oidc" {
		t.Fatalf("disabled config = %v, want exactly enabled:false and provider", conf)
	}
	cases := map[string]*httptest.ResponseRecorder{
		"authorize": serveOAuth(oauthAuthorizeRouter(nil, nil), "GET",
			"/api/v1/auth/oauth/authorize", ""),
		"authorize link": serveOAuth(oauthAuthorizeRouter(nil, nil), "GET",
			"/api/v1/auth/oauth/authorize?intent=link", ""),
		"callback": serveOAuth(oauthCallbackHandler(nil, nil, &config.OAuthConfig{}),
			"GET", "/api/v1/auth/oauth/callback?code=c&state=s", ""),
		"link grant": serveOAuth(oauthLinkGrantHandler(nil, nil), "POST",
			oauthLinkGrantPath, `{"grant":"g"}`),
	}
	for name, w := range cases {
		if w.Code != http.StatusNotFound || decodeAny(t, w)["error"] != "OAuth not configured" {
			t.Errorf("%s with OAuth disabled = %d %s, want 404 OAuth not configured",
				name, w.Code, w.Body)
		}
	}
}

func TestNewRouterOAuthProvider_IdPDownAtStartupStillBuildsAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	SetShutdownContext(ctx)
	t.Cleanup(func() { SetShutdownContext(context.Background()) })

	idp := fakeidp.New(t, "cid")
	idp.FailDiscovery(1, http.StatusServiceUnavailable)
	cfg := config.DefaultConfig()
	cfg.OAuth = discoveryOAuthConfig(idp)
	provider := newRouterOAuthProvider(cfg)
	if provider == nil {
		t.Fatal("an IdP outage at startup must not turn SSO off (nil provider)")
	}
	if ready, reason := provider.Ready(); ready || reason != auth.ReasonIdPUnreachable {
		t.Fatalf("Ready() = %v, %q right after a failed startup discovery", ready, reason)
	}
	if st := CurrentSSODiscoveryStatus(); !st.Enabled || st.Ready || st.Failures != 1 {
		t.Fatalf("status while down = %+v, want enabled, not ready, 1 failure", st)
	}
	// The background loop retries on its own (first backoff is at most 2 s).
	deadline := time.Now().Add(15 * time.Second)
	for ready, _ := provider.Ready(); !ready; ready, _ = provider.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("SSO did not recover after the IdP came back")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := CurrentSSODiscoveryStatus(); !st.Enabled || !st.Ready || st.Failures != 1 {
		t.Fatalf("status after recovery = %+v, want enabled, ready, 1 failure", st)
	}
}

func TestNewRouterOAuthProvider_DisabledIsNilAndUnreported(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.OAuth.Enabled = false
	if p := newRouterOAuthProvider(cfg); p != nil {
		t.Fatalf("OAuth disabled built a provider: %v", p)
	}
	if st := CurrentSSODiscoveryStatus(); st != (SSODiscoveryStatus{}) {
		t.Fatalf("disabled status = %+v, want zero (no SSO metrics)", st)
	}
}

func TestOAuthNotReady_SignedInLinkGets503(t *testing.T) {
	pool := surfacePool(t)
	ctx := context.Background()
	userID, err := auth.CreateUser(ctx, pool, "link-not-ready@fixture.invalid",
		"local-fixture-password", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	session, err := auth.CreateSession(ctx, pool, userID)
	if err != nil {
		t.Fatal(err)
	}
	idp := fakeidp.New(t, "cid")
	idp.FailDiscovery(1, http.StatusBadGateway)
	cfg := discoveryOAuthConfig(idp)
	provider := auth.NewOAuthProvider(&cfg)
	_ = provider.Discover(ctx)
	req := httptest.NewRequest("GET", "/api/v1/auth/oauth/authorize?intent=link", nil)
	req.AddCookie(&http.Cookie{Name: "sage_session", Value: session})
	w := httptest.NewRecorder()
	oauthAuthorizeRouter(provider, pool)(w, req)
	assertSSONotReady(t, "signed-in authorize link", w)
}

// A misconfiguration found at startup is logged at ERROR exactly once: the
// router must not add a second ERROR on top of the provider's own log.
func TestNewRouterOAuthProvider_StartupMisconfigLoggedOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	SetShutdownContext(ctx)
	t.Cleanup(func() { SetShutdownContext(context.Background()) })
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	idp := fakeidp.New(t, "cid")
	idp.SetDiscovery(map[string]any{"issuer": "https://other-issuer.invalid"})
	cfg := config.DefaultConfig()
	cfg.OAuth = discoveryOAuthConfig(idp)
	provider := newRouterOAuthProvider(cfg)
	cancel()
	if provider == nil {
		t.Fatal("a misconfigured IdP must still build the provider")
	}
	if ready, reason := provider.Ready(); ready || reason != auth.ReasonMisconfigured {
		t.Fatalf("Ready() = %v, %q; want false, misconfigured", ready, reason)
	}
	out := logs.String()
	if got := strings.Count(out, `"level":"ERROR"`); got != 1 {
		t.Fatalf("startup ERROR logs = %d, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, "issuer") {
		t.Fatalf("the ERROR log does not name the issuer mismatch:\n%s", out)
	}
}
