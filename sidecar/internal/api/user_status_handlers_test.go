package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-01: PUT /api/v1/users/{id}/status deactivates or re-enables a
// user; the delete handler turns a foreign-key refusal into a readable 409.

func statusMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerUserRoutes(mux, nil)
	return mux
}

func serveStatus(mux http.Handler, id, body string, user *auth.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+id+"/status",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(req, user))
	return w
}

func errorText(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	return body["error"]
}

// The pool is nil: every one of these must be refused before any query.
func TestSetUserStatusHandler_InvalidInput(t *testing.T) {
	cases := []struct{ name, id, body, want string }{
		{"non-numeric id", "abc", `{"disabled":true}`, "invalid user ID"},
		{"zero id", "0", `{"disabled":true}`, "invalid user ID"},
		{"negative id", "-3", `{"disabled":true}`, "invalid user ID"},
		{"missing field", "5", `{}`, "disabled (true or false) is required"},
		{"null field", "5", `{"disabled":null}`, "disabled (true or false) is required"},
		{"wrong type", "5", `{"disabled":"yes"}`, "invalid request body"},
		{"malformed", "5", `{"disabled":`, "invalid request body"},
		{"empty body", "5", ``, "invalid request body"},
	}
	mux := statusMux()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := serveStatus(mux, c.id, c.body, testAdminUser())
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if got := errorText(t, w); got != c.want {
				t.Fatalf("error = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSetUserStatusHandler_RefusesSelfDeactivation(t *testing.T) {
	admin := testAdminUser()
	w := serveStatus(statusMux(), fmt.Sprint(admin.ID), `{"disabled":true}`, admin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if got := errorText(t, w); got != "cannot deactivate your own account" {
		t.Fatalf("error = %q", got)
	}
}

func TestSetUserStatusHandler_AdminOnly(t *testing.T) {
	mux := statusMux()
	for _, u := range []*auth.User{testOperatorUser(), testViewerUser()} {
		w := serveStatus(mux, "9", `{"disabled":true}`, u)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", u.Role, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/9/status",
		strings.NewReader(`{"disabled":true}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status = %d, want 401", w.Code)
	}
}

func TestWriteUserStatusError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		want   string
	}{
		{auth.ErrUserNotFound, http.StatusNotFound, "user not found"},
		{fmt.Errorf("wrapped: %w", auth.ErrLastAdmin), http.StatusForbidden,
			"cannot deactivate the last enabled admin"},
		{errors.New("connection reset"), http.StatusInternalServerError, "internal error"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		writeUserStatusError(w, httptest.NewRequest(http.MethodPut, "/x", nil), c.err)
		if w.Code != c.status {
			t.Fatalf("%v: status = %d, want %d", c.err, w.Code, c.status)
		}
		if got := errorText(t, w); got != c.want {
			t.Fatalf("%v: error = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestWriteDeleteUserError(t *testing.T) {
	fk := &pgconn.PgError{Code: "23503", SchemaName: "sage", TableName: "mystery_refs",
		ConstraintName: "mystery_refs_user_fkey"}
	cases := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"last admin", auth.ErrLastAdmin, http.StatusForbidden, "cannot delete the last admin"},
		{"not found", auth.ErrUserNotFound, http.StatusNotFound, "user not found"},
		{"foreign key", fmt.Errorf("deleting user: %w", fk), http.StatusConflict,
			"user is still referenced by sage.mystery_refs; deactivate the user instead"},
		{"unique violation", &pgconn.PgError{Code: "23505"},
			http.StatusInternalServerError, "internal error"},
		{"plain", errors.New("boom"), http.StatusInternalServerError, "internal error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeDeleteUserError(w, httptest.NewRequest(http.MethodDelete, "/x", nil), c.err)
			if w.Code != c.status {
				t.Fatalf("status = %d, want %d", w.Code, c.status)
			}
			if got := errorText(t, w); got != c.want {
				t.Fatalf("error = %q, want %q", got, c.want)
			}
		})
	}
}

// A 23503 without a table name (an older server) still gets a 409.
func TestWriteDeleteUserError_ForeignKeyWithoutTable(t *testing.T) {
	w := httptest.NewRecorder()
	writeDeleteUserError(w, httptest.NewRequest(http.MethodDelete, "/x", nil),
		&pgconn.PgError{Code: "23503"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := errorText(t, w); got !=
		"user is still referenced by another table; deactivate the user instead" {
		t.Fatalf("error = %q", got)
	}
}

func TestDisabledUserSSOFailure(t *testing.T) {
	if got := loginFailureReason(auth.ErrUserDisabled); got != "user_disabled" {
		t.Fatalf("loginFailureReason = %q, want user_disabled", got)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/callback", nil)
	writeOAuthUserError(w, r, fmt.Errorf("sso: %w", auth.ErrUserDisabled), auth.LinkIntent{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := errorText(t, w); !strings.Contains(got, "deactivated") {
		t.Fatalf("error = %q, want it to say the account is deactivated", got)
	}
}

func TestDatabricksAppMiddleware_RefusesDisabledUser(t *testing.T) {
	mw := databricksAppAuthMiddleware(func(context.Context, string, string) (*auth.User, error) {
		return nil, auth.ErrUserDisabled
	})
	called := false
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil)
	req.Header.Set("X-Forwarded-Email", "gone@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if called || w.Code != http.StatusForbidden {
		t.Fatalf("called=%v status=%d, want refused with 403", called, w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "databricks_app_user_disabled" {
		t.Fatalf("code = %q, want databricks_app_user_disabled", body["code"])
	}
}
