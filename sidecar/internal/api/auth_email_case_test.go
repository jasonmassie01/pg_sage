package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-05: user emails are one identity whatever their case. The create
// endpoint stores the normalized address and checks its format; login and
// its failure audit match any case.

func TestCreateUserRequestValidate_NormalizesAndChecksEmail(t *testing.T) {
	cases := []struct {
		email, wantMsg, wantEmail string
		ssoOnly                   bool
	}{
		{email: " BOB@x.io ", wantEmail: "bob@x.io"},
		{email: "Carol@Corp.com", wantEmail: "carol@corp.com", ssoOnly: true},
		{email: "not-an-email", wantMsg: "invalid email address"},
		{email: "Alice <alice@corp.com>", wantMsg: "invalid email address"},
		{email: "   ", wantMsg: "email and password required"},
		{email: "   ", wantMsg: "email required", ssoOnly: true},
		{email: "nope", wantMsg: "invalid email address", ssoOnly: true},
	}
	for _, c := range cases {
		req := createUserRequest{Email: c.email, Role: auth.RoleViewer, SSOOnly: c.ssoOnly}
		if !c.ssoOnly {
			req.Password = "long-enough-pw"
		}
		msg := req.validate()
		if msg != c.wantMsg {
			t.Errorf("validate(%q, sso=%v) = %q, want %q", c.email, c.ssoOnly, msg, c.wantMsg)
			continue
		}
		if c.wantMsg == "" && req.Email != c.wantEmail {
			t.Errorf("validate(%q) left email %q, want %q", c.email, req.Email, c.wantEmail)
		}
	}
}

func TestCreateUserHandler_RejectsMalformedEmailBeforeStore(t *testing.T) {
	// A nil pool would panic if the handler reached the store.
	for _, email := range []string{"not-an-email", "   ", "a@b.io, c@d.io"} {
		body, _ := json.Marshal(map[string]string{"email": email,
			"password": "long-enough-pw", "role": "viewer"})
		w := doRequest(createUserHandler(nil), "POST", "/api/v1/users", string(body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("create %q = %d, want 400", email, w.Code)
		}
	}
}

func emailCaseLocal(t *testing.T) string {
	return strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
}

func TestCreateUserEndpoint_CaseVariantConflicts(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	f.login(t, auth.RoleAdmin)
	lower := emailCaseLocal(t) + "-bob@x.io"
	create := func(email string) (int, string) {
		body, _ := json.Marshal(map[string]string{"email": email,
			"password": "long-enough-pw", "role": "viewer"})
		return f.request(t, http.MethodPost, "/api/v1/users", string(body))
	}
	if status, body := create(lower); status != http.StatusCreated {
		t.Fatalf("create %s = %d %s", lower, status, body)
	}
	if status, body := create(strings.ToUpper(lower)); status != http.StatusConflict {
		t.Fatalf("create upper-case variant = %d %s, want 409", status, body)
	}
	for _, bad := range []string{"not-an-email", "   "} {
		if status, body := create(bad); status != http.StatusBadRequest {
			t.Fatalf("create %q = %d %s, want 400", bad, status, body)
		}
	}
	mixed := "  " + strings.ToUpper(emailCaseLocal(t)) + "-Carol@X.io "
	if status, body := create(mixed); status != http.StatusCreated {
		t.Fatalf("create mixed-case = %d %s", status, body)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.users
		WHERE email = ANY($1)`, []string{lower, emailCaseLocal(t) + "-carol@x.io"}).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stored normalized accounts = %d, want 2", n)
	}
}

func TestPasswordLoginAnyCaseAndFailureAuditsTarget(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	id, email := f.createUser(t, "alice", auth.RoleOperator)
	upper := strings.ToUpper(email)
	since := f.maxAuditID(t)
	bad, _ := json.Marshal(map[string]string{"email": upper, "password": "wrong-password"})
	if status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login",
		string(bad)); status != http.StatusUnauthorized {
		t.Fatalf("bad password = %d, want 401", status)
	}
	good, _ := json.Marshal(map[string]string{"email": " " + upper,
		"password": "local-disposable-fixture-only"})
	status, body := f.request(t, http.MethodPost, "/api/v1/auth/login", string(good))
	if status != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`"id":%d`, id)) {
		t.Fatalf("upper-case login = %d %s, want 200 for user %d", status, body, id)
	}
	failed := f.auditSince(t, since, auth.AuditLoginFailed)
	if len(failed) != 1 || failed[0].Target != id {
		t.Fatalf("login_failed rows = %+v, want one targeting %d", failed, id)
	}
	ok := f.oneAudit(t, since, auth.AuditLoginSucceeded)
	if ok.Target != id || ok.Actor != id {
		t.Fatalf("login_succeeded row = %+v", ok)
	}
}
