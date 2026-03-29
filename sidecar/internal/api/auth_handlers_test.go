package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// --- meHandler ---

func TestMeHandler_WithUser(t *testing.T) {
	handler := meHandler()
	w := doRequestWithUser(
		handler, "GET", "/api/v1/auth/me", "",
		testAdminUser())

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp["email"] != "admin@test.com" {
		t.Errorf("email: got %v, want admin@test.com",
			resp["email"])
	}
	if resp["role"] != "admin" {
		t.Errorf("role: got %v, want admin", resp["role"])
	}
	if resp["id"].(float64) != 1 {
		t.Errorf("id: got %v, want 1", resp["id"])
	}
}

func TestMeHandler_WithOperator(t *testing.T) {
	handler := meHandler()
	w := doRequestWithUser(
		handler, "GET", "/api/v1/auth/me", "",
		testOperatorUser())

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["role"] != "operator" {
		t.Errorf("role: got %v, want operator", resp["role"])
	}
	if resp["id"].(float64) != 2 {
		t.Errorf("id: got %v, want 2", resp["id"])
	}
}

func TestMeHandler_NoUser(t *testing.T) {
	handler := meHandler()
	w := doRequest(handler, "GET", "/api/v1/auth/me", "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "not authenticated" {
		t.Errorf("error: got %q, want 'not authenticated'",
			resp["error"])
	}
}

// --- loginHandler ---
// loginHandler calls auth.Authenticate and auth.CreateSession which
// require a real *pgxpool.Pool. We test the request parsing and
// validation paths that execute before any DB call.

func TestLoginHandler_MalformedJSON(t *testing.T) {
	handler := loginHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/login",
		"not valid json")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

func TestLoginHandler_EmptyBody(t *testing.T) {
	handler := loginHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/login", "{}")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "email and password required" {
		t.Errorf("error: got %q, want 'email and password "+
			"required'", resp["error"])
	}
}

func TestLoginHandler_MissingEmail(t *testing.T) {
	handler := loginHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/login",
		`{"password":"secret"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "email and password required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestLoginHandler_MissingPassword(t *testing.T) {
	handler := loginHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/login",
		`{"email":"user@test.com"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "email and password required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestLoginHandler_EmptyStrings(t *testing.T) {
	handler := loginHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/login",
		`{"email":"","password":""}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "email and password required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

// --- logoutHandler ---

func TestLogoutHandler_NoCookie(t *testing.T) {
	// logoutHandler with nil pool -- DeleteSession is called
	// only if cookie exists, so nil pool is safe here.
	handler := logoutHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/logout", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "logged out" {
		t.Errorf("status: got %q, want 'logged out'",
			resp["status"])
	}

	// Verify the cookie is cleared.
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "sage_session" {
			found = true
			if c.MaxAge != -1 {
				t.Errorf("cookie MaxAge: got %d, want -1",
					c.MaxAge)
			}
			if c.Value != "" {
				t.Errorf("cookie Value: got %q, want empty",
					c.Value)
			}
			if !c.HttpOnly {
				t.Error("cookie should be HttpOnly")
			}
		}
	}
	if !found {
		t.Error("expected sage_session cookie in response")
	}
}

func TestLogoutHandler_WithCookie(t *testing.T) {
	// With a cookie but nil pool, DeleteSession will panic
	// or error. However, logoutHandler ignores the error from
	// DeleteSession (uses _ = ), so it should still return OK.
	// Actually, nil pool will cause a nil pointer dereference
	// inside auth.DeleteSession. Since we can't mock the pool,
	// we test only the no-cookie path.
	//
	// This is a known limitation: full logout test with cookie
	// requires integration test with a real pgxpool.
	// See: TestLogoutHandler_NoCookie for the no-cookie path.
}

// --- createUserHandler ---

func TestCreateUserHandler_MalformedJSON(t *testing.T) {
	handler := createUserHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/users", "bad json")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

func TestCreateUserHandler_MissingEmail(t *testing.T) {
	handler := createUserHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/users",
		`{"password":"secret"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "email and password required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestCreateUserHandler_MissingPassword(t *testing.T) {
	handler := createUserHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/users",
		`{"email":"a@b.com"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "email and password required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestCreateUserHandler_InvalidRole(t *testing.T) {
	handler := createUserHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/users",
		`{"email":"a@b.com","password":"pw","role":"superuser"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid role" {
		t.Errorf("error: got %q, want 'invalid role'",
			resp["error"])
	}
}

func TestCreateUserHandler_EmptyRoleDefaultsViewer(t *testing.T) {
	// When role is empty, the handler should default it to "viewer"
	// and proceed to the DB call. Since pool is nil, the DB call
	// will panic. We recover from the panic to verify the handler
	// did NOT reject the empty role as invalid (i.e., it got past
	// role validation).
	handler := createUserHandler(nil)

	defer func() {
		if r := recover(); r != nil {
			// Panic at DB layer is expected (nil pool).
			// The important thing is it didn't return 400 "invalid role".
		}
	}()

	w := doRequest(
		handler, "POST", "/api/v1/users",
		`{"email":"a@b.com","password":"pw"}`)

	// If we reach here (no panic), check it didn't reject the role.
	if w.Code == http.StatusBadRequest {
		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["error"] == "invalid role" {
			t.Error("empty role should default to viewer, " +
				"not be rejected as invalid")
		}
	}
}

func TestCreateUserHandler_ValidRoles(t *testing.T) {
	validRoles := []string{"admin", "operator", "viewer"}
	for _, role := range validRoles {
		t.Run(role, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					// Panic at DB layer expected (nil pool).
					// Reaching this means role was accepted.
				}
			}()

			handler := createUserHandler(nil)
			body := `{"email":"a@b.com","password":"pw","role":"` +
				role + `"}`
			w := doRequest(
				handler, "POST", "/api/v1/users", body)

			// Should not fail with "invalid role".
			if w.Code == http.StatusBadRequest {
				var resp map[string]string
				json.NewDecoder(w.Body).Decode(&resp)
				if resp["error"] == "invalid role" {
					t.Errorf("role %q should be valid",
						role)
				}
			}
		})
	}
}

// --- deleteUserHandler ---

func TestDeleteUserHandler_InvalidID(t *testing.T) {
	handler := deleteUserHandler(nil)

	// Use a mux to handle path parameter extraction.
	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/users/{id}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/users/abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid user ID" {
		t.Errorf("error: got %q, want 'invalid user ID'",
			resp["error"])
	}
}

func TestDeleteUserHandler_ZeroID(t *testing.T) {
	// ID "0" should be rejected as invalid (must be >= 1).
	handler := deleteUserHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/users/{id}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/users/0", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

func TestDeleteUserHandler_NegativeID(t *testing.T) {
	// Negative IDs should be rejected.
	handler := deleteUserHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/users/{id}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/users/-5", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

// --- updateUserRoleHandler ---

func TestUpdateUserRoleHandler_InvalidID(t *testing.T) {
	handler := updateUserRoleHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/users/{id}/role", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/users/abc/role",
		strings.NewReader(`{"role":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid user ID" {
		t.Errorf("error: got %q, want 'invalid user ID'",
			resp["error"])
	}
}

func TestUpdateUserRoleHandler_MalformedJSON(t *testing.T) {
	handler := updateUserRoleHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/users/{id}/role", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/users/1/role",
		strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

// --- listUsersHandler ---

// listUsersHandler requires a real pool for auth.ListUsers.
// No request validation to test (GET with no params).
// Full test requires integration test.

// --- Response format validation ---

func TestLoginHandler_ResponseContentType(t *testing.T) {
	handler := loginHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/login", "{}")

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json",
			ct)
	}
}

func TestMeHandler_ResponseContentType(t *testing.T) {
	handler := meHandler()
	w := doRequest(handler, "GET", "/api/v1/auth/me", "")

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json",
			ct)
	}
}

func TestLogoutHandler_ResponseContentType(t *testing.T) {
	handler := logoutHandler(nil)
	w := doRequest(
		handler, "POST", "/api/v1/auth/logout", "")

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json",
			ct)
	}
}

// --- IsValidRole coverage ---

func TestIsValidRole(t *testing.T) {
	tests := []struct {
		role  string
		valid bool
	}{
		{"admin", true},
		{"operator", true},
		{"viewer", true},
		{"superuser", false},
		{"", false},
		{"Admin", false},
		{"ADMIN", false},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			got := auth.IsValidRole(tt.role)
			if got != tt.valid {
				t.Errorf("IsValidRole(%q): got %v, want %v",
					tt.role, got, tt.valid)
			}
		})
	}
}
