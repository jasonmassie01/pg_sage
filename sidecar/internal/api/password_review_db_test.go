package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-03 review: a forced change cannot keep the temporary password,
// and a must-change session cannot start an SSO link either (the authorize
// route is public for login, so its link branch resolves the session itself
// and bypassed the middleware gate).

func TestPasswordChange_SameAsCurrentIs400AndKeepsTheGate(t *testing.T) {
	f := passwordFixture(t)
	f.login(t, auth.RoleAdmin)
	viewerEmail := strings.ToLower(t.Name()) + "-viewer@fixture.invalid"
	viewerID, err := auth.CreateUser(context.Background(), f.pool, viewerEmail,
		pwFixture, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loginLimiter.reset(viewerEmail) })
	temp := resetViaAPI(t, f, viewerID)
	viewer := f.browser(t)
	if status := viewer.signIn(t, viewerEmail, temp); status != http.StatusOK {
		t.Fatalf("temporary password login = %d, want 200", status)
	}
	status, body := viewer.request(t, http.MethodPut, "/api/v1/auth/password",
		pwChangeBody(temp, temp))
	if status != http.StatusBadRequest || !strings.Contains(body, "current password") {
		t.Fatalf("keeping the temporary password = %d %s, want 400 naming the "+
			"current password", status, body)
	}
	if strings.Contains(body, temp) {
		t.Fatalf("the refusal echoes the password: %s", body)
	}
	requireForcedChange(t, viewer)
}

func TestOAuthLinkIntent_RefusedWhilePasswordMustChange(t *testing.T) {
	f := newLinkFixture(t)
	id, email := f.createUser(t, "forced", auth.RoleViewer)
	if _, err := f.pool.Exec(context.Background(),
		"UPDATE sage.users SET must_change_password = true WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if status := f.signIn(t, email, pwFixture); status != http.StatusOK {
		t.Fatalf("login = %d, want 200", status)
	}
	resp := f.raw(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	data, _ := io.ReadAll(resp.Body)
	body := string(data)
	if resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(body, passwordChangeRequiredCode) {
		t.Fatalf("link authorize while forced = %d %s, want 403 %s",
			resp.StatusCode, body, passwordChangeRequiredCode)
	}
	for _, c := range resp.Cookies() {
		if strings.Contains(strings.ToLower(c.Name), "state") && c.Value != "" {
			t.Fatalf("a refused link set the OAuth state cookie %q", c.Name)
		}
	}
	// Plain SSO login through the same route is not a session action and
	// stays available.
	login := f.raw(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if login.StatusCode == http.StatusForbidden {
		t.Fatalf("SSO login authorize = %d, want it unaffected by the gate",
			login.StatusCode)
	}
}
