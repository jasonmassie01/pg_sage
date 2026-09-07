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

func TestWave2ControlledConfigUpdateRequiresExpectedGeneration(t *testing.T) {
	cfg := config.DefaultConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	handler := configUpdateHandler(fleet.NewManager(cfg), cfg, controller)
	req := httptest.NewRequest(
		http.MethodPut, "/api/v1/config",
		strings.NewReader(`{"trust":{"level":"advisory"}}`),
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	if response.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want %d; body=%s",
			response.Code, http.StatusPreconditionRequired, response.Body.String())
	}
	if got := controller.Active(); got.Generation != 1 ||
		got.Config.Trust.Level != cfg.Trust.Level {
		t.Fatalf("active snapshot changed after rejected request: %+v", got)
	}
}

func TestWave2ControlledConfigUpdateUsesCASAndReturnsGenerations(t *testing.T) {
	cfg := config.DefaultConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	handler := configUpdateHandler(fleet.NewManager(cfg), cfg, controller)

	first := wave2ConfigRequest(t, handler,
		`{"expected_generation":1,"trust":{"level":"advisory"}}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body=%s",
			first.Code, first.Body.String())
	}
	var applied config.ApplyResult
	if err := json.NewDecoder(first.Body).Decode(&applied); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if applied.DesiredGeneration != 2 || applied.ActiveGeneration != 2 {
		t.Fatalf("first generations = %+v, want desired=2 active=2", applied)
	}
	if got := controller.Active().Config.Trust.Level; got != "advisory" {
		t.Fatalf("active trust level = %q, want advisory", got)
	}

	stale := wave2ConfigRequest(t, handler,
		`{"expected_generation":1,"trust":{"level":"autonomous"}}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale status = %d, want 409; body=%s",
			stale.Code, stale.Body.String())
	}
	if got := controller.Active(); got.Generation != 2 ||
		got.Config.Trust.Level != "advisory" {
		t.Fatalf("stale request changed active snapshot: %+v", got)
	}
}

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
