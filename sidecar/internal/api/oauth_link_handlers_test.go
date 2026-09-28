package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// D7: SSO identities bind to an existing account only through a signed-in
// link or an admin-issued one-time grant. Email alone never links.

type stubIdP struct {
	server *httptest.Server
	mu     sync.Mutex
	info   map[string]any
}

func newStubIdP(t *testing.T) *stubIdP {
	t.Helper()
	idp := &stubIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter,
		_ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": idp.server.URL + "/auth",
			"token_endpoint":         idp.server.URL + "/token",
			"userinfo_endpoint":      idp.server.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "stub-access"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		idp.mu.Lock()
		defer idp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(idp.info)
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *stubIdP) assert(subject, email string, verified bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.info = map[string]any{"sub": subject, "email": email, "email_verified": verified}
}

type linkFixture struct {
	*surfaceFixture
	idp *stubIdP
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	pool := surfacePool(t)
	idp := newStubIdP(t)
	cfg := config.DefaultConfig()
	cfg.OAuth = config.OAuthConfig{Enabled: true, Provider: "oidc", ClientID: "cid",
		ClientSecret: "stub-secret", RedirectURL: "https://sage.test/cb",
		IssuerURL: idp.server.URL, DefaultRole: auth.RoleViewer}
	return &linkFixture{surfaceFixture: surfaceRouter(t, pool, cfg, nil), idp: idp}
}

func (f *linkFixture) resetBrowser() {
	f.client.Jar, _ = cookiejar.New(nil)
}

func (f *linkFixture) raw(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *linkFixture) userID(t *testing.T, email string) int {
	t.Helper()
	var id int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM sage.users WHERE email=$1`, email).Scan(&id); err != nil {
		t.Fatalf("user %s: %v", email, err)
	}
	return id
}

func (f *linkFixture) subjectOf(t *testing.T, id int) string {
	t.Helper()
	var subject *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT oauth_subject FROM sage.users WHERE id=$1`, id).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject == nil {
		return ""
	}
	return *subject
}

func (f *linkFixture) auditCount(t *testing.T, event string, target int) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.auth_audit
		WHERE event=$1 AND target_user_id=$2`, event, target).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// authorizeState starts an OAuth round trip and returns the issued state.
func (f *linkFixture) authorizeState(t *testing.T, method, path, body string) string {
	t.Helper()
	resp := f.raw(t, method, path, body)
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode authorize: %v", err)
	}
	parsed, err := url.Parse(out.URL)
	if err != nil || parsed.Query().Get("state") == "" {
		t.Fatalf("authorize URL without state: %q", out.URL)
	}
	return parsed.Query().Get("state")
}

func (f *linkFixture) callback(t *testing.T, state string) *http.Response {
	t.Helper()
	return f.raw(t, http.MethodGet,
		"/api/v1/auth/oauth/callback?code=stub-code&state="+url.QueryEscape(state), "")
}

func (f *linkFixture) createUser(t *testing.T, suffix, role string) (int, string) {
	t.Helper()
	email := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + "-" + suffix +
		"@fixture.invalid"
	id, err := auth.CreateUser(context.Background(), f.pool, email,
		"local-disposable-fixture-only", role)
	if err != nil {
		t.Fatal(err)
	}
	return id, email
}

func TestOAuthAuthorizeLinkIntentRequiresSession(t *testing.T) {
	f := newLinkFixture(t)
	resp := f.raw(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous link authorize = %d, want 401", resp.StatusCode)
	}
}

func TestOAuthCallbackLinkIntentBindsSessionUser(t *testing.T) {
	f := newLinkFixture(t)
	email := f.login(t, auth.RoleOperator)
	id := f.userID(t, email)
	f.idp.assert("sub-session-link", strings.ToUpper(email), true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	resp := f.callback(t, state)
	if resp.StatusCode != http.StatusFound ||
		resp.Header.Get("Location") != "/#/profile?linked=1" {
		t.Fatalf("link callback = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := f.subjectOf(t, id); got != "sub-session-link" {
		t.Fatalf("subject = %q, want sub-session-link", got)
	}
	var role string
	_ = f.pool.QueryRow(context.Background(),
		`SELECT role FROM sage.users WHERE id=$1`, id).Scan(&role)
	if role != auth.RoleOperator {
		t.Fatalf("link changed role to %q", role)
	}
	if n := f.auditCount(t, "oidc_linked", id); n != 1 {
		t.Fatalf("oidc_linked audit rows = %d", n)
	}
	var detail string
	_ = f.pool.QueryRow(context.Background(), `SELECT detail::text FROM sage.auth_audit
		WHERE event='oidc_linked' AND target_user_id=$1`, id).Scan(&detail)
	if !strings.Contains(detail, f.idp.server.URL) || strings.Contains(detail, "@") {
		t.Fatalf("link audit detail = %s, want issuer and no email", detail)
	}
	replay := f.callback(t, state)
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed link state = %d, want 401", replay.StatusCode)
	}
}

func TestOAuthCallbackLinkIntentRejectsIdentityBoundElsewhere(t *testing.T) {
	f := newLinkFixture(t)
	otherID, otherEmail := f.createUser(t, "owner", auth.RoleAdmin)
	bound := auth.Identity{Issuer: f.idp.server.URL, Subject: "sub-taken",
		Email: otherEmail, EmailVerified: true}
	if err := auth.LinkOAuthIdentity(context.Background(), f.pool, otherID, bound,
		"oidc"); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	email := f.login(t, auth.RoleViewer)
	f.idp.assert("sub-taken", email, true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusConflict {
		t.Fatalf("link of bound identity = %d, want 409", resp.StatusCode)
	}
	if got := f.subjectOf(t, f.userID(t, email)); got != "" {
		t.Fatalf("second user linked to %q", got)
	}
	if got := f.subjectOf(t, otherID); got != "sub-taken" {
		t.Fatalf("owner identity changed to %q", got)
	}
}

func TestOAuthCallbackLinkIntentRejectsEmailMismatchAndUnverified(t *testing.T) {
	f := newLinkFixture(t)
	email := f.login(t, auth.RoleViewer)
	id := f.userID(t, email)
	f.idp.assert("sub-mismatch", "someone-else@fixture.invalid", true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusConflict {
		t.Fatalf("email mismatch link = %d, want 409", resp.StatusCode)
	}
	f.idp.assert("sub-unverified", email, false)
	state = f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unverified link = %d, want 401", resp.StatusCode)
	}
	if got := f.subjectOf(t, id); got != "" {
		t.Fatalf("rejected identity bound: %q", got)
	}
}

func TestOAuthPlainCallbackStillRefusesPasswordEmail(t *testing.T) {
	f := newLinkFixture(t)
	id, email := f.createUser(t, "pw", auth.RoleAdmin)
	f.idp.assert("sub-plain", email, true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plain SSO login over password email = %d, want 403", resp.StatusCode)
	}
	if got := f.subjectOf(t, id); got != "" {
		t.Fatalf("plain login auto-linked on email: %q", got)
	}
}

func issueGrant(t *testing.T, f *linkFixture, target int) string {
	t.Helper()
	resp := f.raw(t, http.MethodPost,
		"/api/v1/users/"+strconv.Itoa(target)+"/oidc-link-grant", "{}")
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue grant: %d %s", resp.StatusCode, data)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" ||
		out.ExpiresAt == "" {
		t.Fatalf("grant response %s: %v", data, err)
	}
	return out.Token
}

func TestOAuthLinkGrantFlowBindsGrantUser(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f := newLinkFixture(t)
	f.login(t, auth.RoleAdmin)
	email := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + "-sso@fixture.invalid"
	target, err := auth.CreateSSOOnlyUser(context.Background(), f.pool, email,
		auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	token := issueGrant(t, f, target)
	if n := f.auditCount(t, "oidc_link_grant_issued", target); n != 1 {
		t.Fatalf("grant issue audit rows = %d", n)
	}
	f.resetBrowser()
	f.idp.assert("sub-grant", email, true)
	state := f.authorizeState(t, http.MethodPost, "/api/v1/auth/oauth/link-grant",
		`{"grant":"`+token+`"}`)
	resp := f.callback(t, state)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("grant callback = %d", resp.StatusCode)
	}
	if got := f.subjectOf(t, target); got != "sub-grant" {
		t.Fatalf("grant user subject = %q", got)
	}
	if f.auditCount(t, "oidc_link_grant_used", target) != 1 ||
		f.auditCount(t, "oidc_linked", target) != 1 {
		t.Fatal("grant use or link was not audited")
	}
	status, body := f.request(t, http.MethodGet, "/api/v1/auth/me", "")
	if status != http.StatusOK || !strings.Contains(body, email) {
		t.Fatalf("session after grant link: %d %s", status, body)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("link grant token was written to the log")
	}
}

func TestOAuthLinkGrantReplayAndExpiryRejected(t *testing.T) {
	f := newLinkFixture(t)
	adminEmail := f.login(t, auth.RoleAdmin)
	target, _ := f.createUser(t, "target", auth.RoleViewer)
	token := issueGrant(t, f, target)
	f.resetBrowser()
	f.authorizeState(t, http.MethodPost, "/api/v1/auth/oauth/link-grant",
		`{"grant":"`+token+`"}`)
	resp := f.raw(t, http.MethodPost, "/api/v1/auth/oauth/link-grant", `{"grant":"`+token+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed grant = %d, want 400", resp.StatusCode)
	}
	status, body := f.request(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"`+adminEmail+`","password":"local-disposable-fixture-only"}`)
	if status != http.StatusOK {
		t.Fatalf("admin re-login: %d %s", status, body)
	}
	fresh := issueGrant(t, f, target)
	if _, err := f.pool.Exec(context.Background(), `UPDATE sage.user_oidc_link_grants
		SET expires_at = now() - interval '1 second' WHERE user_id=$1`, target); err != nil {
		t.Fatal(err)
	}
	f.resetBrowser()
	resp = f.raw(t, http.MethodPost, "/api/v1/auth/oauth/link-grant", `{"grant":"`+fresh+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired grant = %d, want 400", resp.StatusCode)
	}
}

func TestAdminUnlinkAndGrantRequireAdmin(t *testing.T) {
	f := newLinkFixture(t)
	target, _ := f.createUser(t, "target", auth.RoleViewer)
	f.login(t, auth.RoleOperator)
	path := "/api/v1/users/" + strconv.Itoa(target)
	if resp := f.raw(t, http.MethodDelete, path+"/oidc", ""); resp.StatusCode !=
		http.StatusForbidden {
		t.Fatalf("operator unlink = %d, want 403", resp.StatusCode)
	}
	if resp := f.raw(t, http.MethodPost, path+"/oidc-link-grant", "{}"); resp.StatusCode !=
		http.StatusForbidden {
		t.Fatalf("operator grant = %d, want 403", resp.StatusCode)
	}
}

func TestAdminUnlinkClearsIdentityAndAudits(t *testing.T) {
	f := newLinkFixture(t)
	target, email := f.createUser(t, "linked", auth.RoleOperator)
	if err := auth.LinkOAuthIdentity(context.Background(), f.pool, target,
		auth.Identity{Issuer: f.idp.server.URL, Subject: "sub-unlink", Email: email,
			EmailVerified: true}, "oidc"); err != nil {
		t.Fatal(err)
	}
	admin := f.userID(t, f.login(t, auth.RoleAdmin))
	path := "/api/v1/users/" + strconv.Itoa(target) + "/oidc"
	if resp := f.raw(t, http.MethodDelete, path, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin unlink = %d", resp.StatusCode)
	}
	if got := f.subjectOf(t, target); got != "" {
		t.Fatalf("identity remains: %q", got)
	}
	var actor int
	if err := f.pool.QueryRow(context.Background(), `SELECT actor_user_id
		FROM sage.auth_audit WHERE event='oidc_unlinked' AND target_user_id=$1`,
		target).Scan(&actor); err != nil || actor != admin {
		t.Fatalf("unlink audit actor = %d err=%v, want %d", actor, err, admin)
	}
	if resp := f.raw(t, http.MethodDelete, path, ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlink of unlinked user = %d, want 409", resp.StatusCode)
	}
}

func TestListUsersExposesSSOLinked(t *testing.T) {
	f := newLinkFixture(t)
	linked, email := f.createUser(t, "linked", auth.RoleViewer)
	unlinked, _ := f.createUser(t, "unlinked", auth.RoleViewer)
	if err := auth.LinkOAuthIdentity(context.Background(), f.pool, linked,
		auth.Identity{Issuer: f.idp.server.URL, Subject: "sub-list", Email: email,
			EmailVerified: true}, "oidc"); err != nil {
		t.Fatal(err)
	}
	f.login(t, auth.RoleAdmin)
	status, body := f.request(t, http.MethodGet, "/api/v1/users", "")
	if status != http.StatusOK {
		t.Fatalf("list users: %d %s", status, body)
	}
	var out struct {
		Users []struct {
			ID            int    `json:"id"`
			SSOLinked     bool   `json:"sso_linked"`
			SSOIssuer     string `json:"sso_issuer"`
			PasswordLogin bool   `json:"password_login"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	// The package database is shared, so only this test's users are checked.
	seen := 0
	for _, u := range out.Users {
		switch u.ID {
		case linked:
			seen++
			if !u.SSOLinked || u.SSOIssuer != f.idp.server.URL || !u.PasswordLogin {
				t.Fatalf("linked user reported as %+v", u)
			}
		case unlinked:
			seen++
			if u.SSOLinked || u.SSOIssuer != "" || !u.PasswordLogin {
				t.Fatalf("unlinked user reported as %+v", u)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("list is missing this test's users: %s", body)
	}
}

func TestCreateSSOOnlyUserViaAPI(t *testing.T) {
	f := newLinkFixture(t)
	f.login(t, auth.RoleAdmin)
	email := strings.ToLower(t.Name()) + "-new@fixture.invalid"
	status, body := f.request(t, http.MethodPost, "/api/v1/users",
		`{"email":"`+email+`","role":"operator","sso_only":true}`)
	if status != http.StatusCreated {
		t.Fatalf("create SSO-only user: %d %s", status, body)
	}
	var hash *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT password FROM sage.users WHERE email=$1`, email).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != nil {
		t.Fatal("SSO-only user was stored with a password")
	}
	status, _ = f.request(t, http.MethodPost, "/api/v1/users",
		`{"email":"x-`+email+`","role":"viewer","sso_only":true,"password":"longenough1"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("SSO-only user with a password = %d, want 400", status)
	}
	status, _ = f.request(t, http.MethodPost, "/api/v1/users",
		`{"email":"y-`+email+`","role":"viewer"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("password user without password = %d, want 400", status)
	}
}

func TestSSOStatusEndpoint(t *testing.T) {
	f := newLinkFixture(t)
	email := f.login(t, auth.RoleViewer)
	status, body := f.request(t, http.MethodGet, "/api/v1/auth/sso", "")
	if status != http.StatusOK || !strings.Contains(body, `"linked":false`) ||
		!strings.Contains(body, `"oauth_enabled":true`) {
		t.Fatalf("sso status before link: %d %s", status, body)
	}
	if err := auth.LinkOAuthIdentity(context.Background(), f.pool, f.userID(t, email),
		auth.Identity{Issuer: f.idp.server.URL, Subject: "sub-status", Email: email,
			EmailVerified: true}, "oidc"); err != nil {
		t.Fatal(err)
	}
	status, body = f.request(t, http.MethodGet, "/api/v1/auth/sso", "")
	if status != http.StatusOK || !strings.Contains(body, `"linked":true`) {
		t.Fatalf("sso status after link: %d %s", status, body)
	}
	f.resetBrowser()
	if status, _ = f.request(t, http.MethodGet, "/api/v1/auth/sso", ""); status !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous sso status = %d, want 401", status)
	}
}
