package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestWave2ControlledConfigGetReportsDesiredAndActiveGenerations(t *testing.T) {
	cfg := config.DefaultConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++
	if _, err := controller.Apply(t.Context(), 1, candidate); err != nil {
		t.Fatalf("apply restart-pending candidate: %v", err)
	}
	handler := configGetHandler(fleet.NewManager(cfg), cfg, controller)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))

	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["desired_generation"] != float64(2) ||
		body["active_generation"] != float64(1) {
		t.Fatalf("generation response = %+v, want desired=2 active=1", body)
	}
}

type apiTrustOwner struct{}

func (apiTrustOwner) Name() string { return "trust_policy" }

func (apiTrustOwner) Prepare(
	context.Context, config.ConfigSnapshot, config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	return apiPreparedConfig{}, nil
}

type apiPreparedConfig struct{}

func (apiPreparedConfig) Commit(context.Context) error   { return nil }
func (apiPreparedConfig) Rollback(context.Context) error { return nil }
func (apiPreparedConfig) Drain(context.Context) error    { return nil }

func wave2ConfigRequest(
	t *testing.T, handler http.Handler, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPut, "/api/v1/config", strings.NewReader(body),
	))
	return response
}
