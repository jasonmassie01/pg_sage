package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/pg-sage/sidecar/internal/auth"
)

// withUser creates a request with an authenticated user in context.
func withUser(
	r *http.Request, user *auth.User,
) *http.Request {
	ctx := context.WithValue(
		r.Context(), userContextKey, user)
	return r.WithContext(ctx)
}

// testAdminUser returns a test user with admin role.
func testAdminUser() *auth.User {
	return &auth.User{
		ID:    1,
		Email: "admin@test.com",
		Role:  auth.RoleAdmin,
	}
}

// testOperatorUser returns a test user with operator role.
func testOperatorUser() *auth.User {
	return &auth.User{
		ID:    2,
		Email: "operator@test.com",
		Role:  auth.RoleOperator,
	}
}

// testViewerUser returns a test user with viewer role.
func testViewerUser() *auth.User {
	return &auth.User{
		ID:    3,
		Email: "viewer@test.com",
		Role:  auth.RoleViewer,
	}
}

// doRequest sends a request to a handler and returns the recorder.
func doRequest(
	handler http.HandlerFunc,
	method, path, body string,
) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(
			method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// doRequestWithUser sends a request with an authenticated user.
func doRequestWithUser(
	handler http.HandlerFunc,
	method, path, body string,
	user *auth.User,
) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(
			method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req = withUser(req, user)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}
