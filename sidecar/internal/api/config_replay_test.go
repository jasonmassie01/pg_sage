package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-02 (a): replay skips what the override store would refuse today.
func TestReplayStoredOverridesSkipsInvalidRows(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	candidate := config.DefaultConfig()
	var warnings []string
	ReplayStoredOverrides(candidate, []store.ConfigOverride{
		{Key: "collector.interval_seconds", Value: "abc", UpdatedAt: at},
		{Key: "analyzer.unused_index_window_days", Value: "0", UpdatedAt: at},
		{Key: "tuner.llm_enabled", Value: "false", UpdatedAt: at},
		{Key: "trust.level", Value: "advisory", UpdatedAt: at},
		{Key: "advisor.enabled", Value: "False", UpdatedAt: at},
	}, func(msg string) { warnings = append(warnings, msg) })

	if got := candidate.Collector.IntervalSeconds; got != 60 {
		t.Errorf("collector.interval_seconds = %d, want the default 60", got)
	}
	if got := candidate.Analyzer.UnusedIndexWindowDays; got != 7 {
		t.Errorf("unused_index_window_days = %d, want the default 7", got)
	}
	if !candidate.Tuner.LLMEnabled {
		t.Error("stored tuner.llm_enabled (not a settable key) was applied")
	}
	if candidate.Trust.Level != "advisory" || candidate.Advisor.Enabled {
		t.Errorf("valid rows lost: trust=%q advisor.enabled=%v",
			candidate.Trust.Level, candidate.Advisor.Enabled)
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings = %q, want one per skipped row", warnings)
	}
	for i, key := range []string{"collector.interval_seconds",
		"analyzer.unused_index_window_days", "tuner.llm_enabled"} {
		if !strings.Contains(warnings[i], key) ||
			!strings.Contains(warnings[i], "2026-01-02T03:04:05Z") {
			t.Errorf("warning %q does not name %s and its updated_at", warnings[i], key)
		}
	}
	if strings.Contains(warnings[0], "abc") {
		t.Errorf("warning repeats the stored value: %q", warnings[0])
	}
	if err := candidate.Validate(); err != nil {
		t.Fatalf("replayed candidate fails validation: %v", err)
	}
}

// Safety first: an invalid stored window keeps failing closed.
func TestReplayStoredOverridesClosesAnInvalidWindow(t *testing.T) {
	candidate := config.DefaultConfig()
	candidate.Trust.MaintenanceWindow = "* * * * *"
	var warnings []string
	ReplayStoredOverrides(candidate, []store.ConfigOverride{
		{Key: "trust.maintenance_window", Value: "sometimes"},
	}, func(msg string) { warnings = append(warnings, msg) })
	if candidate.Trust.MaintenanceWindow != "never" {
		t.Fatalf("window = %q, want never", candidate.Trust.MaintenanceWindow)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "trust.maintenance_window") {
		t.Fatalf("warnings = %q, want one naming trust.maintenance_window", warnings)
	}
}

func TestReplayStoredOverridesNilAndEmpty(t *testing.T) {
	candidate := config.DefaultConfig()
	calls := 0
	ReplayStoredOverrides(candidate, nil, func(string) { calls++ })
	ReplayStoredOverrides(candidate, []store.ConfigOverride{}, nil)
	ReplayStoredOverrides(candidate, []store.ConfigOverride{
		{Key: "collector.interval_seconds", Value: "abc"},
	}, nil) // a nil warn must not panic
	if calls != 0 || candidate.Collector.IntervalSeconds != 60 {
		t.Fatalf("calls=%d interval=%d, want 0 and 60", calls,
			candidate.Collector.IntervalSeconds)
	}
}

// config-02 (d), boundary: every registry bool applies "True", "TRUE" and
// "1" as true and displays true.
func TestEveryBoolSetterAppliesParseBoolForms(t *testing.T) {
	keys := 0
	for key, vtype := range store.AllowedConfigKeysSnapshot() {
		if vtype != "bool" && vtype != "bool_auto" { // bool_auto: rca-09b
			continue
		}
		keys++
		for _, v := range []string{"True", "TRUE", "1"} {
			candidate := config.DefaultConfig()
			hotReload(candidate, key, "false")
			hotReload(candidate, key, v)
			entry := store.ConfigReadModel(candidate)[key].(map[string]any)
			if entry["value"] != true {
				t.Errorf("%s=%q displays %v, want true", key, v, entry["value"])
			}
		}
		candidate := config.DefaultConfig()
		hotReload(candidate, key, "true")
		hotReload(candidate, key, "0")
		if store.ConfigReadModel(candidate)[key].(map[string]any)["value"] != false {
			t.Errorf("%s=\"0\" did not apply false", key)
		}
	}
	if keys < 20 {
		t.Fatalf("only %d bool keys in the registry; the test lost its subject", keys)
	}
}

// CHECK-22 / config-02 (c): an invalid candidate on PUT is a 400 naming
// the key, not a 500.
func TestPutWithInvalidCandidateReturns400NamingTheKey(t *testing.T) {
	base := config.DefaultConfig()
	base.Collector.IntervalSeconds = 0 // the state an "abc" replay left behind
	controller := config.NewConfigController(base, nil, apiTrustOwner{})
	put := configGlobalPutHandler(nil, controller)
	resp := doRequestWithUser(put, http.MethodPut, "/api/v1/config/global",
		`{"trust.level":"advisory","expected_generation":1}`,
		&auth.User{ID: 1, Role: auth.RoleAdmin})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "collector.interval_seconds") {
		t.Fatalf("body %s does not name collector.interval_seconds", resp.Body.String())
	}
	if controller.Desired().Generation != 1 {
		t.Fatal("a rejected PUT advanced the desired generation")
	}
}

func TestWriteConfigApplyErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{fmt.Errorf("x: %w", config.ErrInvalidCandidate), http.StatusBadRequest},
		{fmt.Errorf("x: %w", config.ErrGenerationConflict), http.StatusConflict},
		{errors.New("connection refused"), http.StatusInternalServerError},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/v1/config/global", nil)
		writeConfigApplyError(w, r, tc.err)
		if w.Code != tc.code {
			t.Errorf("%v -> %d, want %d", tc.err, w.Code, tc.code)
		}
		if tc.code == http.StatusInternalServerError &&
			strings.Contains(w.Body.String(), "connection refused") {
			t.Errorf("internal error text leaked: %s", w.Body.String())
		}
	}
}
