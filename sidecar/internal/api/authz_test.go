package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// E1 (CG-03, group mapping, audit, break-glass) over the real router, a real
// database and an in-process signing IdP.

// fixtureBreakGlassHash is a cost-4 bcrypt hash of the throwaway fixture
// string "fixture-break-glass-pw"; it guards nothing.
const (
	fixtureBreakGlassHash = "$2a$04$C1ODnQFDul1rqzdJ52pN1uFXtmzyOArkugPCm1mrPJqtPc4WJgGey"
	fixtureBreakGlassPW   = "fixture-break-glass-pw"
)

type auditRow struct {
	Event    string
	Actor    int
	Target   int
	SourceIP string
	Detail   map[string]any
}

func newAuthzFixture(t *testing.T, mutate func(*config.Config),
	rt *RuntimeDeps) *linkFixture {
	t.Helper()
	pool := surfacePool(t)
	idp := newStubIdP(t)
	cfg := config.DefaultConfig()
	cfg.OAuth = config.OAuthConfig{Enabled: true, Provider: "oidc", ClientID: "cid",
		ClientSecret: "stub-secret", RedirectURL: "https://sage.test/cb",
		IssuerURL: idp.server.URL, DefaultRole: auth.RoleViewer,
		GroupsClaim: "groups", UnmappedUsers: config.UnmappedUsersDeny}
	if mutate != nil {
		mutate(cfg)
	}
	return &linkFixture{surfaceFixture: surfaceRouter(t, pool, cfg, rt), idp: idp}
}

func withMapping(cfg *config.Config) {
	cfg.OAuth.RoleMapping = []config.OAuthRoleMapping{
		{Group: "pg-admins", Role: auth.RoleAdmin},
		{Group: "pg-viewers", Role: auth.RoleViewer},
	}
}

func (f *linkFixture) maxAuditID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(id), 0) FROM sage.auth_audit`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *linkFixture) auditSince(t *testing.T, since int64, event string) []auditRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT event, actor_user_id,
		target_user_id, source_ip, detail::text FROM sage.auth_audit
		WHERE id > $1 AND event = $2 ORDER BY id`, since, event)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var detail string
		if err := rows.Scan(&r.Event, &r.Actor, &r.Target, &r.SourceIP, &detail); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(detail), &r.Detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "@") || strings.Contains(detail, "fixture-break") ||
			strings.Contains(detail, "local-disposable") {
			t.Fatalf("audit detail leaks an email or secret: %s", detail)
		}
		out = append(out, r)
	}
	return out
}

func (f *linkFixture) oneAudit(t *testing.T, since int64, event string) auditRow {
	t.Helper()
	rows := f.auditSince(t, since, event)
	if len(rows) != 1 {
		t.Fatalf("%s audit rows since %d = %d (%+v), want 1", event, since, len(rows), rows)
	}
	if rows[0].SourceIP != "127.0.0.1" {
		t.Fatalf("%s source_ip = %q, want 127.0.0.1", event, rows[0].SourceIP)
	}
	return rows[0]
}

func (f *linkFixture) sessionCookie() string {
	u, _ := url.Parse(f.server.URL)
	for _, c := range f.client.Jar.Cookies(u) {
		if c.Name == "sage_session" {
			return c.Value
		}
	}
	return ""
}

func TestE1_03_ForgedNonceCallbackGets401(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	since := f.maxAuditID(t)
	f.idp.SetClaims(map[string]any{"sub": "sub-forged", "nonce": "forged-nonce",
		"email": "forged@fixture.invalid", "email_verified": true})
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	resp := f.callback(t, state)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("E1-03: forged nonce callback = %d %s, want 401", resp.StatusCode, body)
	}
	if f.sessionCookie() != "" {
		t.Fatal("E1-03: a forged nonce produced a session")
	}
	var n int
	_ = f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.users WHERE oauth_subject = 'sub-forged'`).Scan(&n)
	if n != 0 {
		t.Fatal("E1-03: a forged nonce created a user")
	}
	row := f.oneAudit(t, since, auth.AuditLoginFailed)
	if row.Detail["reason"] != "nonce_mismatch" || row.Detail["method"] != "oidc" {
		t.Fatalf("login_failed detail = %v, want nonce_mismatch via oidc", row.Detail)
	}
}

func TestOIDCCallbackRejectsTokenWithoutValidSignature(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	f.idp.ForgeSignature(true)
	f.idp.assert("sub-forged-sig", "sig@fixture.invalid", true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged signature callback = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCLoginAuditsSuccess(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	since := f.maxAuditID(t)
	f.idp.assert("sub-audit-ok", "audit-ok@fixture.invalid", true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	resp := f.callback(t, state)
	if resp.StatusCode != http.StatusFound || f.sessionCookie() == "" {
		t.Fatalf("login = %d, cookie %q; want 302 with a session", resp.StatusCode,
			f.sessionCookie())
	}
	row := f.oneAudit(t, since, auth.AuditLoginSucceeded)
	id := f.userID(t, "audit-ok@fixture.invalid")
	if row.Target != id || row.Actor != id || row.Detail["method"] != "oidc" {
		t.Fatalf("login_succeeded row = %+v, want actor=target=%d via oidc", row, id)
	}
}

func TestOIDCGroupMappingAssignsAdmin(t *testing.T) {
	f := newAuthzFixture(t, withMapping, nil)
	f.idp.SetClaims(map[string]any{"sub": "sub-mapped-admin",
		"email": "mapped-admin@fixture.invalid", "email_verified": true,
		"groups": []string{"engineering", "pg-admins"}})
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusFound {
		t.Fatalf("mapped login = %d", resp.StatusCode)
	}
	status, body := f.request(t, http.MethodGet, "/api/v1/auth/me", "")
	if status != http.StatusOK || !strings.Contains(body, `"role":"admin"`) {
		t.Fatalf("me = %d %s, want role admin", status, body)
	}
	if status, _ := f.request(t, http.MethodGet, "/api/v1/users", ""); status != http.StatusOK {
		t.Fatalf("mapped admin GET /users = %d, want 200", status)
	}
}

func TestOIDCUnmappedUserDenied(t *testing.T) {
	f := newAuthzFixture(t, withMapping, nil)
	since := f.maxAuditID(t)
	f.idp.SetClaims(map[string]any{"sub": "sub-unmapped",
		"email": "unmapped@fixture.invalid", "email_verified": true,
		"groups": []string{"engineering"}})
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	resp := f.callback(t, state)
	if resp.StatusCode != http.StatusForbidden || f.sessionCookie() != "" {
		t.Fatalf("unmapped login = %d, cookie %q; want 403 and no session",
			resp.StatusCode, f.sessionCookie())
	}
	var n int
	_ = f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.users WHERE oauth_subject = 'sub-unmapped'`).Scan(&n)
	if n != 0 {
		t.Fatal("an unmapped user was provisioned")
	}
	row := f.oneAudit(t, since, auth.AuditLoginFailed)
	if row.Detail["reason"] != "not_authorized" {
		t.Fatalf("login_failed reason = %v, want not_authorized", row.Detail["reason"])
	}

	f.resetBrowser()
	state = f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	assertErrorPage(t, f.browserCallback(t, state), "/#/login?sso_error=not_authorized")
}

func TestOIDCUnmappedUserGetsDefaultRoleWhenConfigured(t *testing.T) {
	f := newAuthzFixture(t, func(c *config.Config) {
		withMapping(c)
		c.OAuth.UnmappedUsers = config.UnmappedUsersDefaultRole
	}, nil)
	f.idp.SetClaims(map[string]any{"sub": "sub-unmapped-default",
		"email": "unmapped-default@fixture.invalid", "email_verified": true})
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusFound {
		t.Fatalf("login = %d, want 302", resp.StatusCode)
	}
	if _, body := f.request(t, http.MethodGet, "/api/v1/auth/me", ""); !strings.Contains(
		body, `"role":"viewer"`) {
		t.Fatalf("me = %s, want viewer", body)
	}
}

func TestOIDCRoleSyncedFromGroupsOnEachLogin(t *testing.T) {
	f := newAuthzFixture(t, withMapping, nil)
	f.createUser(t, "keeper", auth.RoleAdmin) // never the last admin
	claims := func(groups ...string) map[string]any {
		return map[string]any{"sub": "sub-sync", "email": "sync@fixture.invalid",
			"email_verified": true, "groups": groups}
	}
	f.idp.SetClaims(claims("pg-admins"))
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusFound {
		t.Fatalf("first login = %d", resp.StatusCode)
	}
	id := f.userID(t, "sync@fixture.invalid")
	since := f.maxAuditID(t)
	f.resetBrowser()
	f.idp.SetClaims(claims("pg-viewers"))
	state = f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.callback(t, state); resp.StatusCode != http.StatusFound {
		t.Fatalf("second login = %d", resp.StatusCode)
	}
	if role, _ := auth.UserRole(context.Background(), f.pool, id); role != auth.RoleViewer {
		t.Fatalf("role after IdP demotion = %q, want viewer", role)
	}
	row := f.oneAudit(t, since, auth.AuditUserRoleChanged)
	if row.Target != id || row.Detail["old_role"] != "admin" ||
		row.Detail["new_role"] != "viewer" || row.Detail["source"] != "idp" {
		t.Fatalf("role change audit = %+v", row)
	}
}

func TestPasswordLoginSuccessAndFailureAudited(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	id, email := f.createUser(t, "pw", auth.RoleOperator)
	since := f.maxAuditID(t)
	bad, _ := json.Marshal(map[string]string{"email": email, "password": "wrong-password"})
	if status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login", string(bad)); status !=
		http.StatusUnauthorized {
		t.Fatalf("bad password = %d", status)
	}
	unknown, _ := json.Marshal(map[string]string{"email": "nobody@fixture.invalid",
		"password": "whatever-password"})
	_, _ = f.request(t, http.MethodPost, "/api/v1/auth/login", string(unknown))
	good, _ := json.Marshal(map[string]string{"email": email,
		"password": "local-disposable-fixture-only"})
	if status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login", string(good)); status !=
		http.StatusOK {
		t.Fatalf("good password = %d", status)
	}
	failed := f.auditSince(t, since, auth.AuditLoginFailed)
	if len(failed) != 2 || failed[0].Target != id || failed[1].Target != 0 {
		t.Fatalf("login_failed rows = %+v, want known user %d then unknown (0)", failed, id)
	}
	for _, r := range failed {
		if r.Detail["method"] != "password" || r.Detail["reason"] != "invalid_credentials" ||
			r.SourceIP != "127.0.0.1" {
			t.Fatalf("login_failed row = %+v", r)
		}
	}
	ok := f.oneAudit(t, since, auth.AuditLoginSucceeded)
	if ok.Target != id || ok.Actor != id || ok.Detail["method"] != "password" {
		t.Fatalf("login_succeeded row = %+v", ok)
	}
}

func TestUserAndRoleChangesAudited(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	f.login(t, auth.RoleAdmin)
	adminID := f.userID(t, strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))+
		"@fixture.invalid")
	since := f.maxAuditID(t)
	create := `{"email":"audited-new@fixture.invalid","password":"long-enough-pw","role":"viewer"}`
	status, body := f.request(t, http.MethodPost, "/api/v1/users", create)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	newID := f.userID(t, "audited-new@fixture.invalid")
	path := fmt.Sprintf("/api/v1/users/%d", newID)
	if status, _ := f.request(t, http.MethodPut, path+"/role", `{"role":"operator"}`); status !=
		http.StatusOK {
		t.Fatalf("role change = %d", status)
	}
	if status, _ := f.request(t, http.MethodDelete, path, ""); status != http.StatusOK {
		t.Fatalf("delete = %d", status)
	}
	created := f.oneAudit(t, since, auth.AuditUserCreated)
	if created.Actor != adminID || created.Target != newID || created.Detail["role"] != "viewer" {
		t.Fatalf("user_created = %+v", created)
	}
	changed := f.oneAudit(t, since, auth.AuditUserRoleChanged)
	if changed.Actor != adminID || changed.Detail["old_role"] != "viewer" ||
		changed.Detail["new_role"] != "operator" || changed.Detail["source"] != "api" {
		t.Fatalf("user_role_changed = %+v", changed)
	}
	deleted := f.oneAudit(t, since, auth.AuditUserDeleted)
	if deleted.Actor != adminID || deleted.Target != newID {
		t.Fatalf("user_deleted = %+v", deleted)
	}
}

func TestRejectedRoleChangeIsNotAudited(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	f.login(t, auth.RoleAdmin)
	since := f.maxAuditID(t)
	_, _ = f.request(t, http.MethodPut, "/api/v1/users/987654/role", `{"role":"viewer"}`)
	_, _ = f.request(t, http.MethodPut, "/api/v1/users/987654/role", `{"role":"god"}`)
	if rows := f.auditSince(t, since, auth.AuditUserRoleChanged); len(rows) != 0 {
		t.Fatalf("failed role changes audited: %+v", rows)
	}
}

// slackCapture is a webhook receiver standing in for a Slack channel.
type slackCapture struct {
	mu     sync.Mutex
	bodies []string
}

func newSlackCapture(t *testing.T) (*slackCapture, *httptest.Server) {
	t.Helper()
	c := &slackCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *slackCapture) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

func breakGlassFixture(t *testing.T, enabled bool) (*linkFixture, *slackCapture) {
	t.Helper()
	capture, srv := newSlackCapture(t)
	rt := &RuntimeDeps{NotificationTargetPolicy: notify.TargetPolicy{AllowPrivate: true}}
	f := newAuthzFixture(t, func(c *config.Config) {
		c.OAuth.BreakGlass = config.BreakGlassConfig{Enabled: enabled,
			PasswordHash: fixtureBreakGlassHash}
	}, rt)
	name := "bg-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO sage.notification_channels
		(name, type, config, enabled) VALUES ($1, 'slack', jsonb_build_object('webhook_url',
		$2::text), true)`, name, srv.URL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM sage.notification_channels WHERE name = $1`, name)
		loginLimiter.reset(breakGlassLimiterKey("127.0.0.1"))
	})
	loginLimiter.reset(breakGlassLimiterKey("127.0.0.1"))
	return f, capture
}

func TestBreakGlassDisabledIsNotFound(t *testing.T) {
	f, capture := breakGlassFixture(t, false)
	status, _ := f.request(t, http.MethodPost, "/api/v1/auth/break-glass",
		`{"password":"`+fixtureBreakGlassPW+`"}`)
	if status != http.StatusNotFound || f.sessionCookie() != "" {
		t.Fatalf("disabled break-glass = %d, want 404 and no session", status)
	}
	if len(capture.received()) != 0 {
		t.Fatal("a disabled break-glass attempt raised an alert")
	}
}

func TestBreakGlassWrongPasswordDeniedAndAudited(t *testing.T) {
	f, _ := breakGlassFixture(t, true)
	since := f.maxAuditID(t)
	for _, body := range []string{`{"password":"wrong"}`, `{}`, `not json`} {
		status, _ := f.request(t, http.MethodPost, "/api/v1/auth/break-glass", body)
		if status != http.StatusUnauthorized && status != http.StatusBadRequest {
			t.Fatalf("body %q = %d, want 401/400", body, status)
		}
	}
	if f.sessionCookie() != "" {
		t.Fatal("failed break-glass attempts produced a session")
	}
	rows := f.auditSince(t, since, auth.AuditBreakGlassFailed)
	if len(rows) < 2 || rows[0].SourceIP != "127.0.0.1" {
		t.Fatalf("break_glass_login_failed rows = %+v, want >= 2 with source ip", rows)
	}
}

func TestBreakGlassLoginGrantsAdminAuditsAndAlerts(t *testing.T) {
	f, capture := breakGlassFixture(t, true)
	since := f.maxAuditID(t)
	status, body := f.request(t, http.MethodPost, "/api/v1/auth/break-glass",
		`{"password":"`+fixtureBreakGlassPW+`"}`)
	if status != http.StatusOK || f.sessionCookie() == "" {
		t.Fatalf("break-glass = %d %s, want 200 with a session", status, body)
	}
	if status, _ := f.request(t, http.MethodGet, "/api/v1/users", ""); status != http.StatusOK {
		t.Fatalf("break-glass session GET /users = %d, want 200 (admin)", status)
	}
	row := f.oneAudit(t, since, auth.AuditBreakGlassLogin)
	if row.Target <= 0 || row.Actor != row.Target {
		t.Fatalf("break_glass_login row = %+v", row)
	}
	got := capture.received()
	if len(got) != 1 || !strings.Contains(strings.ToLower(got[0]), "break-glass") ||
		!strings.Contains(got[0], "127.0.0.1") || strings.Contains(got[0], fixtureBreakGlassPW) {
		t.Fatalf("alert payloads = %v, want one break-glass alert naming the source", got)
	}
	var logged int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.notification_log
		WHERE event = $1 AND status = 'sent'`, notify.EventSecurityBreakGlass).Scan(&logged)
	if logged < 1 {
		t.Fatal("break-glass alert missing from notification_log")
	}
}

func TestBreakGlassIsRateLimited(t *testing.T) {
	f, _ := breakGlassFixture(t, true)
	if loginRateLimitDisabled {
		t.Skip("PG_SAGE_DISABLE_LOGIN_RATE_LIMIT=1 disables the limiter under test")
	}
	last := 0
	for i := 0; i < 6; i++ {
		last, _ = f.request(t, http.MethodPost, "/api/v1/auth/break-glass",
			`{"password":"wrong"}`)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("6th attempt = %d, want 429", last)
	}
	status, _ := f.request(t, http.MethodPost, "/api/v1/auth/break-glass",
		`{"password":"`+fixtureBreakGlassPW+`"}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("correct password while limited = %d, want 429", status)
	}
}

func TestClientIPResolverIsUsedForAudit(t *testing.T) {
	SetClientIPResolver(func(*http.Request) string { return "198.51.100.7" })
	t.Cleanup(func() { SetClientIPResolver(nil) })
	f := newAuthzFixture(t, nil, nil)
	_, email := f.createUser(t, "ip", auth.RoleViewer)
	since := f.maxAuditID(t)
	bad, _ := json.Marshal(map[string]string{"email": email, "password": "nope-nope"})
	_, _ = f.request(t, http.MethodPost, "/api/v1/auth/login", string(bad))
	rows := f.auditSince(t, since, auth.AuditLoginFailed)
	if len(rows) != 1 || rows[0].SourceIP != "198.51.100.7" {
		t.Fatalf("rows = %+v, want source ip from the resolver", rows)
	}
}
