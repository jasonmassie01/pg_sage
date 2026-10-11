package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// D7 follow-up: the OAuth callback is a top-level browser navigation, so a
// browser gets redirected to a dashboard page that explains the failure
// instead of a raw JSON body. Non-browser clients keep the JSON statuses.

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func (f *linkFixture) browserCallback(t *testing.T, state string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.server.URL+
		"/api/v1/auth/oauth/callback?code=code-"+url.QueryEscape(state)+
		"&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", browserAccept)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func assertErrorPage(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
		t.Fatalf("callback = %d Location=%q body=%s, want 302 to %s",
			resp.StatusCode, resp.Header.Get("Location"), body, want)
	}
}

func TestOAuthBrowserCallbackLinkRequiredRedirectsToLogin(t *testing.T) {
	f := newLinkFixture(t)
	_, email := f.createUser(t, "pw", auth.RoleAdmin)
	f.idp.assert("sub-page-required", email, true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	assertErrorPage(t, f.browserCallback(t, state), "/#/login?sso_error=link_required")
	if status, _ := f.request(t, http.MethodGet, "/api/v1/auth/me", ""); status != 401 {
		t.Fatalf("refused SSO login created a session: me=%d", status)
	}
}

func TestOAuthBrowserCallbackUnverifiedRedirectsToLogin(t *testing.T) {
	f := newLinkFixture(t)
	f.idp.assert("sub-page-unverified", "someone@fixture.invalid", false)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	assertErrorPage(t, f.browserCallback(t, state), "/#/login?sso_error=unverified")
}

func TestOAuthBrowserCallbackReplayRedirectsToLogin(t *testing.T) {
	f := newLinkFixture(t)
	f.idp.assert("sub-page-replay", "replay@fixture.invalid", true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	if resp := f.browserCallback(t, state); resp.StatusCode != http.StatusFound {
		t.Fatalf("first callback = %d", resp.StatusCode)
	}
	assertErrorPage(t, f.browserCallback(t, state), "/#/login?sso_error=failed")
}

func TestOAuthBrowserCallbackLinkConflictRedirectsToProfile(t *testing.T) {
	f := newLinkFixture(t)
	f.login(t, auth.RoleViewer)
	f.idp.assert("sub-page-conflict", "not-the-account@fixture.invalid", true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize?intent=link", "")
	assertErrorPage(t, f.browserCallback(t, state), "/#/profile?sso_error=link_conflict")
}

func TestOAuthBrowserCallbackGrantConflictRedirectsToLogin(t *testing.T) {
	f := newLinkFixture(t)
	f.login(t, auth.RoleAdmin)
	target, _ := f.createUser(t, "grant", auth.RoleViewer)
	token := issueGrant(t, f, target)
	f.resetBrowser()
	f.idp.assert("sub-page-grant", "wrong-person@fixture.invalid", true)
	state := f.authorizeState(t, http.MethodPost, "/api/v1/auth/oauth/link-grant",
		`{"grant":"`+token+`"}`)
	assertErrorPage(t, f.browserCallback(t, state), "/#/login?sso_error=link_conflict")
}

func TestOAuthNonBrowserCallbackKeepsJSONStatus(t *testing.T) {
	f := newLinkFixture(t)
	_, email := f.createUser(t, "pw", auth.RoleAdmin)
	f.idp.assert("sub-page-json", email, true)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	resp := f.callback(t, state)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(resp.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(string(body), "already exists") {
		t.Fatalf("API client callback = %d %s", resp.StatusCode, body)
	}
}
