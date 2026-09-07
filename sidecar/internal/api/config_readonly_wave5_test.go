package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestWave5YAMLFleetConfigIsReadableButNotWritable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	cfg.Databases = []config.DatabaseConfig{{
		Name: "orders", ExecutionMode: "manual", TrustLevel: "observation",
	}}
	controller := config.NewConfigController(cfg, nil)
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "orders",
		DatabaseID: 7,
		Config:     cfg.Databases[0],
	})

	mux := http.NewServeMux()
	registerConfigRoutesRuntime(
		mux, nil, cfg, mgr, controller, cfg, true,
	)
	admin := &auth.User{ID: 1, Role: "admin"}

	global := wave5ConfigRequest(mux, admin, http.MethodGet,
		"/api/v1/config/global", "")
	if global.Code != http.StatusOK {
		t.Fatalf("global GET status = %d, want 200; body=%s",
			global.Code, global.Body.String())
	}
	var globalBody map[string]any
	if err := json.NewDecoder(global.Body).Decode(&globalBody); err != nil {
		t.Fatalf("decode global response: %v", err)
	}
	if globalBody["read_only"] != true {
		t.Fatalf("global read_only = %#v, want true", globalBody["read_only"])
	}
	configBody, ok := globalBody["config"].(map[string]any)
	if !ok || configBody["collector.interval_seconds"] == nil {
		t.Fatalf("global response omits effective config: %#v", globalBody)
	}

	database := wave5ConfigRequest(mux, admin, http.MethodGet,
		"/api/v1/config/databases/7", "")
	if database.Code != http.StatusOK {
		t.Fatalf("database GET status = %d, want 200; body=%s",
			database.Code, database.Body.String())
	}
	var databaseBody struct {
		ReadOnly bool `json:"read_only"`
		Config   map[string]struct {
			Value any `json:"value"`
		} `json:"config"`
	}
	if err := json.NewDecoder(database.Body).Decode(&databaseBody); err != nil {
		t.Fatalf("decode database response: %v", err)
	}
	if !databaseBody.ReadOnly {
		t.Fatal("database response is not marked read-only")
	}
	if got := databaseBody.Config["execution_mode"].Value; got != "manual" {
		t.Fatalf("execution mode = %#v, want manual", got)
	}
	if got := databaseBody.Config["trust.level"].Value; got != "observation" {
		t.Fatalf("trust level = %#v, want observation", got)
	}

	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/api/v1/config/global", `{}`},
		{http.MethodDelete, "/api/v1/config/global/trust.level", ""},
		{http.MethodPut, "/api/v1/config/databases/7", `{}`},
		{http.MethodGet, "/api/v1/config/audit", ""},
	} {
		response := wave5ConfigRequest(
			mux, admin, request.method, request.path, request.body,
		)
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s status = %d, want 503; body=%s",
				request.method, request.path, response.Code, response.Body.String())
		}
	}
}

func wave5ConfigRequest(
	handler http.Handler, user *auth.User, method, path, body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = withUser(request, user)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
