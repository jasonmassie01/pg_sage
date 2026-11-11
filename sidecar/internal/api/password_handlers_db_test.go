package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// apiauth-03 on real PostgreSQL through the full router: a user changes
// their own password (other sessions end, the calling one survives); an
// admin resets one (all sessions end, a temporary password is shown once
// and the account is held to the change form until it is replaced).

const pwFixture = "local-disposable-fixture-only"

func passwordFixture(t *testing.T) *linkFixture {
	t.Helper()
	pool := surfacePool(t)
	return &linkFixture{surfaceFixture: surfaceRouter(t, pool, config.DefaultConfig(), nil)}
}

// browser returns a second client (its own cookie jar) on the fixture.
func (f *linkFixture) browser(t *testing.T) *linkFixture {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := *f.client
	client.Jar = jar
	sf := *f.surfaceFixture
	sf.client = &client
	return &linkFixture{surfaceFixture: &sf}
}

func (f *linkFixture) signIn(t *testing.T, email, password string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login", string(body))
	return status
}

func pwChangeBody(current, next string) string {
	body, _ := json.Marshal(map[string]string{"current_password": current,
		"new_password": next})
	return string(body)
}

func (f *linkFixture) setSession(t *testing.T, token string) {
	t.Helper()
	u, _ := url.Parse(f.server.URL)
	f.client.Jar.SetCookies(u, []*http.Cookie{{Name: "sage_session", Value: token,
		Path: "/"}})
}

func TestPasswordChange_HappyPathEndsOtherSessions(t *testing.T) {
	f := passwordFixture(t)
	email := f.login(t, auth.RoleViewer)
	t.Cleanup(func() { loginLimiter.reset(email) })
	id := f.userID(t, email)
	other := f.browser(t)
	if status := other.signIn(t, email, pwFixture); status != http.StatusOK {
		t.Fatalf("second session login = %d", status)
	}
	since := f.maxAuditID(t)
	const next = "brand-new-password-1"
	status, body := f.request(t, http.MethodPut, "/api/v1/auth/password",
		pwChangeBody(pwFixture, next))
	if status != http.StatusOK {
		t.Fatalf("change = %d %s, want 200", status, body)
	}
	if status, _ := f.request(t, http.MethodGet, "/api/v1/auth/me", ""); status != 200 {
		t.Fatalf("calling session after change = %d, want 200", status)
	}
	if status, _ := other.request(t, http.MethodGet, "/api/v1/auth/me", ""); status != 401 {
		t.Fatalf("other session after change = %d, want 401", status)
	}
	fresh := f.browser(t)
	if status := fresh.signIn(t, email, pwFixture); status != http.StatusUnauthorized {
		t.Fatalf("old password login = %d, want 401", status)
	}
	if status := fresh.signIn(t, email, next); status != http.StatusOK {
		t.Fatalf("new password login = %d, want 200", status)
	}
	row := f.oneAudit(t, since, auth.AuditPasswordChanged)
	if row.Actor != id || row.Target != id {
		t.Fatalf("password_changed row = %+v, want actor=target=%d", row, id)
	}
	requireNoSecretInAudit(t, f, since, email, next, pwFixture)
}

func requireNoSecretInAudit(t *testing.T, f *linkFixture, since int64, secrets ...string) {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT row_to_json(a)::text FROM sage.auth_audit a WHERE id > $1`, since)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if s != "" && strings.Contains(strings.ToLower(line), strings.ToLower(s)) {
				t.Fatalf("auth_audit row carries a secret or email: %s", line)
			}
		}
	}
}

func TestPasswordChange_InvalidInputIs400(t *testing.T) {
	f := passwordFixture(t)
	email := f.login(t, auth.RoleViewer)
	t.Cleanup(func() { loginLimiter.reset(email) })
	cases := map[string]string{
		"11 bytes":        pwChangeBody(pwFixture, "eleven-byte"),
		"73 bytes":        pwChangeBody(pwFixture, strings.Repeat("q", 73)),
		"the email":       pwChangeBody(pwFixture, email),
		"the email, uc":   pwChangeBody(pwFixture, strings.ToUpper(email)),
		"empty object":    `{}`,
		"missing new":     `{"current_password":"` + pwFixture + `"}`,
		"missing current": `{"new_password":"long-enough-new-pw"}`,
		"not json":        `not json`,
	}
	for name, body := range cases {
		status, resp := f.request(t, http.MethodPut, "/api/v1/auth/password", body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d %s, want 400", name, status, resp)
		}
	}
	if status := f.browser(t).signIn(t, email, pwFixture); status != http.StatusOK {
		t.Fatalf("a refused change broke the old password: login = %d", status)
	}
}

func TestPasswordChange_WrongCurrentCountsAgainstTheLimiter(t *testing.T) {
	f := passwordFixture(t)
	email := f.login(t, auth.RoleViewer)
	t.Cleanup(func() { loginLimiter.reset(email) })
	since := f.maxAuditID(t)
	for i := 1; i <= loginMaxAttempts; i++ {
		status, body := f.request(t, http.MethodPut, "/api/v1/auth/password",
			pwChangeBody("wrong-current-pw", "long-enough-new-pw"))
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d %s, want 401", i, status, body)
		}
	}
	status, body := f.request(t, http.MethodPut, "/api/v1/auth/password",
		pwChangeBody(pwFixture, "long-enough-new-pw"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d %s, want 429", loginMaxAttempts+1, status, body)
	}
	if n := len(f.auditSince(t, since, auth.AuditPasswordChanged)); n != 0 {
		t.Fatalf("%d password_changed rows after only refusals, want 0", n)
	}
	// The calling session is not ended by refusals.
	if status, _ := f.request(t, http.MethodGet, "/api/v1/auth/me", ""); status != 200 {
		t.Fatalf("session after refusals = %d, want 200", status)
	}
}

func TestPasswordChange_SSOOnlyUserGets409(t *testing.T) {
	f := passwordFixture(t)
	ctx := context.Background()
	email := strings.ToLower(t.Name()) + "@fixture.invalid"
	id, err := auth.CreateSSOOnlyUser(ctx, f.pool, email, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.CreateSession(ctx, f.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	f.setSession(t, token)
	status, body := f.request(t, http.MethodPut, "/api/v1/auth/password",
		pwChangeBody("", "long-enough-new-pw"))
	if status != http.StatusConflict || !strings.Contains(body, "no password to change") {
		t.Fatalf("SSO-only change = %d %s, want 409 no password to change", status, body)
	}
	status, body = f.request(t, http.MethodGet, "/api/v1/auth/me", "")
	if status != 200 || !strings.Contains(body, `"password_login":false`) {
		t.Fatalf("me for SSO-only = %d %s, want password_login false", status, body)
	}
}

func TestPasswordReset_ForcesChangeBeforeAnythingElse(t *testing.T) {
	f := passwordFixture(t)
	adminEmail := f.login(t, auth.RoleAdmin)
	adminID := f.userID(t, adminEmail)
	viewerEmail := strings.ToLower(t.Name()) + "-viewer@fixture.invalid"
	viewerID, err := auth.CreateUser(context.Background(), f.pool, viewerEmail,
		pwFixture, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	viewer := f.browser(t)
	if status := viewer.signIn(t, viewerEmail, pwFixture); status != 200 {
		t.Fatalf("viewer login = %d", status)
	}
	since := f.maxAuditID(t)
	temp := resetViaAPI(t, f, viewerID)
	if status, _ := viewer.request(t, http.MethodGet, "/api/v1/auth/me", ""); status != 401 {
		t.Fatalf("viewer session after reset = %d, want 401", status)
	}
	if status := viewer.signIn(t, viewerEmail, pwFixture); status != 401 {
		t.Fatalf("old password after reset = %d, want 401", status)
	}
	if status := viewer.signIn(t, viewerEmail, temp); status != 200 {
		t.Fatalf("temporary password login = %d, want 200", status)
	}
	requireForcedChange(t, viewer)
	status, body := viewer.request(t, http.MethodPut, "/api/v1/auth/password",
		pwChangeBody(temp, "viewer-chosen-password"))
	if status != http.StatusOK {
		t.Fatalf("forced change = %d %s, want 200", status, body)
	}
	if status, body := viewer.request(t, http.MethodGet, "/api/v1/cases", ""); status ==
		http.StatusForbidden || status == http.StatusUnauthorized {
		t.Fatalf("cases after the change = %d %s, want the gate lifted", status, body)
	}
	row := f.oneAudit(t, since, auth.AuditPasswordReset)
	if row.Actor != adminID || row.Target != viewerID {
		t.Fatalf("password_reset row = %+v, want actor %d target %d", row, adminID, viewerID)
	}
	requireNoSecretInAudit(t, f, since, temp, viewerEmail,
		"viewer-chosen-password")
}

// resetViaAPI resets userID's password as the fixture's admin and returns
// the temporary password, checking its shape.
func resetViaAPI(t *testing.T, f *linkFixture, userID int) string {
	t.Helper()
	status, body := f.request(t, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/password-reset", userID), `{}`)
	if status != http.StatusOK {
		t.Fatalf("reset = %d %s, want 200", status, body)
	}
	var resp struct {
		Temporary  string `json:"temporary_password"`
		MustChange bool   `json:"must_change_password"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil ||
		len(resp.Temporary) != 32 || !resp.MustChange {
		t.Fatalf("reset body %s: temporary password of %d chars, want 32 (%v)", body,
			len(resp.Temporary), err)
	}
	return resp.Temporary
}

// requireForcedChange: me works and reports the flag; everything else
// except the change form and logout is refused with password_change_required.
func requireForcedChange(t *testing.T, f *linkFixture) {
	t.Helper()
	status, body := f.request(t, http.MethodGet, "/api/v1/auth/me", "")
	if status != 200 || !strings.Contains(body, `"must_change_password":true`) {
		t.Fatalf("me while forced = %d %s, want must_change_password true", status, body)
	}
	for _, path := range []string{"/api/v1/cases", "/api/v1/databases", "/api/v1/auth/sso"} {
		status, body := f.request(t, http.MethodGet, path, "")
		if status != http.StatusForbidden || !strings.Contains(body,
			"password_change_required") {
			t.Fatalf("%s while forced = %d %s, want 403 password_change_required",
				path, status, body)
		}
	}
}

func TestPasswordReset_Refusals(t *testing.T) {
	f := passwordFixture(t)
	adminEmail := f.login(t, auth.RoleAdmin)
	adminID := f.userID(t, adminEmail)
	ctx := context.Background()
	ssoID, err := auth.CreateSSOOnlyUser(ctx, f.pool,
		strings.ToLower(t.Name())+"-sso@fixture.invalid", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path   string
		status int
	}{
		{fmt.Sprintf("/api/v1/users/%d/password-reset", adminID), http.StatusForbidden},
		{fmt.Sprintf("/api/v1/users/%d/password-reset", ssoID), http.StatusConflict},
		{"/api/v1/users/999999/password-reset", http.StatusNotFound},
		{"/api/v1/users/abc/password-reset", http.StatusBadRequest},
		{"/api/v1/users/0/password-reset", http.StatusBadRequest},
	}
	for _, c := range cases {
		if status, body := f.request(t, http.MethodPost, c.path, `{}`); status != c.status {
			t.Fatalf("%s = %d %s, want %d", c.path, status, body, c.status)
		}
	}
	if status := f.browser(t).signIn(t, adminEmail, pwFixture); status != 200 {
		t.Fatalf("a refused self-reset changed the admin's password: login = %d", status)
	}
}

func TestPasswordReset_NonAdminIsForbidden(t *testing.T) {
	f := passwordFixture(t)
	email := f.login(t, auth.RoleOperator)
	id := f.userID(t, email)
	status, _ := f.request(t, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/password-reset", id+1), `{}`)
	if status != http.StatusForbidden {
		t.Fatalf("operator reset = %d, want 403", status)
	}
}

func TestCreateUser_PolicyIs400AndDuplicateIs409(t *testing.T) {
	f := passwordFixture(t)
	f.login(t, auth.RoleAdmin)
	buf := captureAccessLog(t)
	email := strings.ToLower(t.Name()) + "-new@fixture.invalid"
	create := func(pw string) (int, string) {
		body, _ := json.Marshal(map[string]string{"email": email, "password": pw,
			"role": "viewer"})
		return f.request(t, http.MethodPost, "/api/v1/users", string(body))
	}
	for _, pw := range []string{"eleven-byte", strings.Repeat("w", 73), email} {
		if status, body := create(pw); status != http.StatusBadRequest {
			t.Fatalf("create with %d-byte password = %d %s, want 400", len(pw), status, body)
		}
	}
	if status, body := create("twelve-bytes"); status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, body)
	}
	if status, body := create("twelve-bytes"); status != http.StatusConflict {
		t.Fatalf("duplicate = %d %s, want 409", status, body)
	}
	if strings.Contains(buf.String(), email) || strings.Contains(buf.String(), "twelve-bytes") {
		t.Fatalf("logs carry the email or password:\n%s", buf.String())
	}
}
