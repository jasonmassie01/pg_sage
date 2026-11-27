package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-10: every mode builds a config controller, so the router refuses
// to start without one instead of serving a legacy write path.

func TestNewRouter_NilRuntimeIsAnError(t *testing.T) {
	handler, err := NewRouterFullRuntime(
		nil, config.DefaultConfig(), nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("NewRouterFullRuntime(nil runtime) returned no error")
	}
	if handler != nil {
		t.Fatalf("handler = %T, want nil on a construction error", handler)
	}
	if !strings.Contains(err.Error(), "config controller") {
		t.Fatalf("error = %q, want it to name the missing config controller", err)
	}
}

func TestNewRouter_RuntimeWithoutControllerIsAnError(t *testing.T) {
	handler, err := NewRouterFullRuntime(
		nil, config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{DisableConfigWrites: true})
	if err == nil || handler != nil {
		t.Fatalf("NewRouterFullRuntime without controller = (%v, %v), want (nil, error)",
			handler, err)
	}
}

func TestNewRouter_WithControllerServes(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	controller := config.NewConfigController(cfg, nil)
	// GET /api/v1/config is viewer-gated (apiauth-09): sign the caller in.
	handler, err := NewRouterFullRuntime(
		nil, cfg, nil, nil, nil, nil,
		&RuntimeDeps{ConfigController: controller}, injectCharRole)
	if err != nil {
		t.Fatalf("NewRouterFullRuntime: %v", err)
	}
	if handler == nil {
		t.Fatal("handler is nil")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.Header.Set("X-Char-Role", "viewer")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/config = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Trust            config.TrustConfig `json:"trust"`
		ActiveGeneration uint64             `json:"active_generation"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Trust.Level != "advisory" || body.ActiveGeneration != 1 {
		t.Fatalf("config = %+v, want the controller's advisory generation 1", body)
	}
}
