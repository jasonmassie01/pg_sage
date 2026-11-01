package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestDeploymentStatusUsesActiveSnapshotAndNeverSerializesConfigSecrets(t *testing.T) {
	active := config.DefaultConfig()
	active.LLM.APIKey = "secret-test-ai"
	active.LLM.OptimizerLLM.APIKey = "secret-test-optimizer"
	active.LLM.Endpoint = "https://user:secret-url-password@workspace.example/v1?token=secret-url-query"
	active.SRE.SLO.Push.HMACSecret = "secret-test-push"
	active.SRE.ChangeEvents.HMACSecret = "secret-test-ingest"
	active.SRE.Autonomy.GameDays.LocalDSN = "postgres://secret-test-local-dsn"
	active.SRE.SLO.Prometheus.URL = "https://secret-test-prometheus.example"
	active.SRE.SLO.Prometheus.BearerToken = "secret-test-prometheus-token"
	active.Postgres.Password = "secret-test-database-password"
	active.EncryptionKey = "secret-test-encryption"
	active.SRE.AutomaticStart = false
	stale := config.DefaultConfig()
	h := deploymentStatusHandler(stale, RuntimeDeps{ConfigController: config.NewConfigController(active, nil), DatabricksAppAuth: true}, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deployment/status", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if strings.Contains(w.Body.String(), "secret-") || strings.Contains(w.Body.String(), "postgres://") {
		t.Fatalf("configuration secret escaped projection: %s", w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["source"] != "controller_active" || result["runtime_liveness"] != "not_reported" || result["sre"].(map[string]any)["automatic_start"] != false {
		t.Fatalf("status did not use/qualify active snapshot: %s", w.Body.String())
	}
	if result["ai"].(map[string]any)["general"].(map[string]any)["endpoint_origin"] != "https://workspace.example" {
		t.Fatal("endpoint was not reduced to origin")
	}
}

func TestDeploymentAndTelemetryStatusRequireRolesBeforeReadingRuntime(t *testing.T) {
	reads := 0
	mux := http.NewServeMux()
	registerDeploymentStatusRoutes(mux, config.DefaultConfig(), RuntimeDeps{TelemetryStatus: func() any { reads++; return map[string]any{"mode": "metadata_only", "state": "disabled"} }}, nil)
	for _, tc := range []struct {
		path, role string
		want       int
	}{
		{"/api/v1/deployment/status", "", http.StatusUnauthorized},
		{"/api/v1/deployment/status", auth.RoleViewer, http.StatusForbidden},
		{"/api/v1/deployment/status", auth.RoleAdmin, http.StatusOK},
		{"/api/v1/lakebase-observability/status", "", http.StatusUnauthorized},
		{"/api/v1/lakebase-observability/status", auth.RoleViewer, http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.role != "" {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, &auth.User{ID: 1, Role: tc.role}))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s role=%q status=%d want=%d", tc.path, tc.role, w.Code, tc.want)
		}
	}
	if reads != 1 {
		t.Fatalf("unauthorized request read runtime: reads=%d", reads)
	}
}

func TestTelemetryHistoryAuthenticatesAndBoundsScopeBeforeRead(t *testing.T) {
	reads := 0
	callback := func(_ context.Context, database, stream string, from, to time.Time, limit int) (any, error) {
		reads++
		if database != "orders" || stream != "plan_history" || limit != 100 || to.Sub(from) != time.Hour {
			t.Fatal("incorrect trusted history request")
		}
		return map[string]any{"historical_only": true, "events": []any{}}, nil
	}
	mux := http.NewServeMux()
	registerDeploymentStatusRoutes(mux, config.DefaultConfig(), RuntimeDeps{TelemetryHistory: callback}, nil)
	q := url.Values{"database": {"orders"}, "stream": {"plan_history"}, "from": {"2026-10-29T00:00:00Z"}, "to": {"2026-10-29T01:00:00Z"}}
	request := func(role string, values url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/lakebase-observability/history?"+values.Encode(), nil)
		if role != "" {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, &auth.User{ID: 1, Role: role}))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := request("", q); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	if w := request(auth.RoleViewer, q); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	if reads != 0 {
		t.Fatal("unauthorized history callback")
	}
	for _, change := range []struct{ key, value string }{
		{"database", ""}, {"database", "all"}, {"database", "orders;select"}, {"stream", "arbitrary_table"}, {"from", ""}, {"from", "yesterday"}, {"to", "2026-10-20T00:00:00Z"}, {"to", "2026-11-29T00:00:00Z"}, {"limit", ""}, {"limit", "0"}, {"limit", "1001"}, {"limit", "not-a-number"}, {"sql", "SELECT secret"},
	} {
		copy := url.Values{}
		for k, v := range q {
			copy[k] = append([]string{}, v...)
		}
		copy.Set(change.key, change.value)
		if w := request(auth.RoleAdmin, copy); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s status=%d", change.key, w.Code)
		}
	}
	duplicate := url.Values{}
	for k, v := range q {
		duplicate[k] = append([]string{}, v...)
	}
	duplicate.Add("database", "billing")
	if w := request(auth.RoleAdmin, duplicate); w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}
	if reads != 0 {
		t.Fatal("invalid request reached history callback")
	}
	if w := request(auth.RoleAdmin, q); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if reads != 1 {
		t.Fatal("valid history not read exactly once")
	}
}

func TestTelemetryHistoryNeverEchoesStorageErrors(t *testing.T) {
	h := telemetryHistoryHandler(RuntimeDeps{TelemetryHistory: func(context.Context, string, string, time.Time, time.Time, int) (any, error) {
		return nil, errors.New("secret-database-payload")
	}})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/lakebase-observability/history?database=orders&stream=plan_history&from=2026-10-29T00:00:00Z&to=2026-10-29T01:00:00Z", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "secret-") {
		t.Fatalf("history error exposed storage data: %d %s", w.Code, w.Body.String())
	}
}
