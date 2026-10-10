package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/auth"
)

// Spec §8.3: GET /api/v1/agent-environments/{database} (operator) and
// PUT (admin; two admins when widening → 202 pending). An unknown database
// is 404, a malformed label 422, a refused binding 409 with the reason,
// and no control database 503.

const envTestDatabaseID = "00000000-0000-4000-8000-0000000e0a01"

func newEnvAPI(t *testing.T, withControl bool) *http.ServeMux {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.guard_environment_labels
			WHERE database_id = $1`, envTestDatabaseID)
	}
	clean()
	t.Cleanup(clean)
	control := pool
	if !withControl {
		control = nil
	}
	svc := &envbind.Service{Binder: envbind.NewBinder(control, nil),
		Resolve: func(_ context.Context, name string) (envbind.Database, error) {
			if name != "testdb" {
				return envbind.Database{}, envbind.ErrUnknownDatabase
			}
			return envbind.Database{ID: envTestDatabaseID, Name: name, Pool: pool}, nil
		}}
	mux := http.NewServeMux()
	registerAgentEnvRoutes(mux, svc)
	return mux
}

func adminNamed(email string) *auth.User {
	return &auth.User{ID: len(email), Email: email, Role: "admin"}
}

func doJSON(t *testing.T, mux *http.ServeMux, user *auth.User, method, path string,
	body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestAgentEnvRoutes_RolesAndDefault(t *testing.T) {
	mux := newEnvAPI(t, true)
	path := "/api/v1/agent-environments/testdb"
	if code, _ := doJSON(t, mux, nil, "GET", path, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET", path, nil); code != http.StatusForbidden {
		t.Fatalf("viewer: %d", code)
	}
	code, body := doJSON(t, mux, operatorUser(), "GET", path, nil)
	if code != http.StatusOK || body["label"] != "prod" || body["effective"] != "prod" ||
		body["verified"] != false || body["identity"] == nil {
		t.Fatalf("operator GET: %d %v", code, body)
	}
	if code, _ := doJSON(t, mux, operatorUser(), "PUT", path,
		map[string]string{"label": "dev"}); code != http.StatusForbidden {
		t.Fatalf("operator PUT: %d", code)
	}
	if code, _ := doJSON(t, mux, operatorUser(), "GET",
		"/api/v1/agent-environments/nope", nil); code != http.StatusNotFound {
		t.Fatalf("unknown database: %d", code)
	}
}

func TestAgentEnvRoutes_WideningTwoAdmins(t *testing.T) {
	mux := newEnvAPI(t, true)
	path := "/api/v1/agent-environments/testdb"
	code, body := doJSON(t, mux, adminNamed("alice@x"), "PUT", path,
		map[string]string{"label": "dev"})
	if code != http.StatusAccepted || body["pending"] != true ||
		body["pending_label"] != "dev" {
		t.Fatalf("first admin: %d %v", code, body)
	}
	code, body = doJSON(t, mux, operatorUser(), "GET", path, nil)
	if code != http.StatusOK || body["effective"] != "prod" {
		t.Fatalf("pending is not effective: %d %v", code, body)
	}
	code, body = doJSON(t, mux, adminNamed("bob@x"), "PUT", path,
		map[string]string{"label": "dev"})
	if code != http.StatusOK || body["label"] != "dev" || body["effective"] != "dev" ||
		body["verified"] != true || body["set_by"] != "bob@x" {
		t.Fatalf("second admin: %d %v", code, body)
	}
	code, body = doJSON(t, mux, adminNamed("carol@x"), "PUT", path,
		map[string]string{"label": "prod"})
	if code != http.StatusOK || body["effective"] != "prod" {
		t.Fatalf("narrowing: %d %v", code, body)
	}
}

func TestAgentEnvRoutes_Errors(t *testing.T) {
	mux := newEnvAPI(t, true)
	path := "/api/v1/agent-environments/testdb"
	for _, body := range []any{map[string]string{"label": "qa"},
		map[string]any{"label": "dev", "extra": 1}, "not an object"} {
		if code, _ := doJSON(t, mux, adminNamed("a@x"), "PUT", path, body); code !=
			http.StatusUnprocessableEntity {
			t.Errorf("body %v: %d", body, code)
		}
	}
	code, body := doJSON(t, mux, adminNamed("a@x"), "PUT", path,
		map[string]string{"label": "branch"})
	if code != http.StatusConflict || body["verdict"] != "blocked" ||
		body["reason_code"] != envbind.ReasonReceiptMissing || body["fix"] == "" {
		t.Fatalf("branch without receipt: %d %v", code, body)
	}
	noControl := newEnvAPI(t, false)
	if code, body := doJSON(t, noControl, adminNamed("a@x"), "PUT", path,
		map[string]string{"label": "dev"}); code != http.StatusServiceUnavailable ||
		body["code"] != "unavailable" {
		t.Fatalf("no control database: %d %v", code, body)
	}
	nilSvc := http.NewServeMux()
	registerAgentEnvRoutes(nilSvc, nil)
	if code, _ := doJSON(t, nilSvc, operatorUser(), "GET", path, nil); code !=
		http.StatusServiceUnavailable {
		t.Fatalf("no service: %d", code)
	}
}
