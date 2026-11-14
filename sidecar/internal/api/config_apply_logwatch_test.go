package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// rca-09b: logwatch.enabled takes true, false or auto through the API.
// No concurrent access tests: hotReload mutates a config the caller owns
// under the controller's lock; these functions keep no shared state.

// logWatchSetting reads logwatch.enabled back in the API's grammar.
func logWatchSetting(c *config.Config) string { return c.LogWatch.Setting() }

// State transitions: auto -> on -> off -> auto through hot reload.
func TestHotReloadLogWatchEnabledModes(t *testing.T) {
	cfg := config.DefaultConfig()
	if logWatchSetting(cfg) != "auto" {
		t.Fatalf("default = %q, want auto", logWatchSetting(cfg))
	}
	for _, step := range []struct{ value, mode string }{
		{"true", config.LogWatchOn}, {"false", config.LogWatchOff},
		{"auto", config.LogWatchAuto}, {"TRUE", config.LogWatchOn},
		{"AUTO", config.LogWatchAuto},
	} {
		hotReload(cfg, "logwatch.enabled", step.value)
		if got := cfg.LogWatch.Mode(); got != step.mode {
			t.Fatalf("after %q mode = %q, want %q", step.value, got, step.mode)
		}
	}
}

// Invalid input: the override API refuses anything but true, false or
// auto, naming the key; a value reaching the setter unvalidated fails
// closed to off.
func TestLogWatchEnabledOverrideValidation(t *testing.T) {
	for _, v := range []string{"true", "false", "auto", "AUTO"} {
		if err := store.ValidateConfigOverride("logwatch.enabled", v); err != nil {
			t.Fatalf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"yes-please", "", "maybe", "on"} {
		err := store.ValidateConfigOverride("logwatch.enabled", v)
		if err == nil || !strings.Contains(err.Error(), "logwatch.enabled") {
			t.Fatalf("%q: err = %v, want a refusal naming logwatch.enabled", v, err)
		}
	}
	cfg := config.DefaultConfig()
	hotReload(cfg, "logwatch.enabled", "yes-please")
	if cfg.LogWatch.Mode() != config.LogWatchOff {
		t.Fatalf("unvalidated value gave mode %q, want off", cfg.LogWatch.Mode())
	}
}

// The read model shows auto as "auto" and an explicit switch as a bool.
func TestConfigReadModelLogWatchEnabled(t *testing.T) {
	cfg := config.DefaultConfig()
	if got := logWatchReadValue(t, cfg); got != "auto" {
		t.Fatalf("default read value = %#v, want \"auto\"", got)
	}
	off := false
	cfg.LogWatch.Enabled = &off
	if got := logWatchReadValue(t, cfg); got != false {
		t.Fatalf("explicit false read value = %#v, want false", got)
	}
}

func logWatchReadValue(t *testing.T, cfg *config.Config) any {
	t.Helper()
	model := store.ConfigReadModel(cfg)
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return findReadValue(t, decoded, "logwatch.enabled")
}

// findReadValue finds key's value in the read model: either a flat
// {"key": {"value": v}} entry or a nested section.
func findReadValue(t *testing.T, model map[string]any, key string) any {
	t.Helper()
	entry, ok := model[key]
	if !ok {
		section, rest, _ := strings.Cut(key, ".")
		nested, isMap := model[section].(map[string]any)
		if !isMap {
			t.Fatalf("read model has no %s: %v", key, model)
		}
		entry = nested[rest]
	}
	if field, isMap := entry.(map[string]any); isMap {
		return field["value"]
	}
	return entry
}

// The deployment status reports the mode, so an operator sees auto.
func TestDeploymentStatusReportsLogWatchMode(t *testing.T) {
	for _, c := range []struct {
		enabled *bool
		mode    string
		on      bool
	}{{nil, "auto", true}, {boolRef(true), "on", true}, {boolRef(false), "off", false}} {
		active := config.DefaultConfig()
		active.LogWatch.Enabled = c.enabled
		h := deploymentStatusHandler(config.DefaultConfig(), RuntimeDeps{
			ConfigController: config.NewConfigController(active, nil)}, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deployment/status", nil))
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("status %d: %v", w.Code, err)
		}
		readers, _ := result["readers"].(map[string]any)
		if readers["logwatch_mode"] != c.mode || readers["logwatch_enabled"] != c.on {
			t.Fatalf("readers = %v, want logwatch_mode %s enabled %v", readers, c.mode, c.on)
		}
	}
}

func boolRef(b bool) *bool { return &b }
