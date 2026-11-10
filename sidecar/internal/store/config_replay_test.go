package store

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: these helpers are pure functions of their
// arguments and read only the immutable key registry.

// config-02 (d): the API accepts every form config.ParseBool accepts.
func TestValidateBoolAcceptsParseBoolForms(t *testing.T) {
	for _, v := range []string{"true", "True", "TRUE", "1", "t", "false", "False", "0", "f"} {
		if err := ValidateConfigOverride("llm.enabled", v); err != nil {
			t.Errorf("llm.enabled=%q rejected: %v", v, err)
		}
	}
	for _, v := range []string{"", "yes", "on", "2", "TRUE "} {
		err := ValidateConfigOverride("llm.enabled", v)
		if err == nil || !strings.Contains(err.Error(), "must be true or false") {
			t.Errorf("llm.enabled=%q err = %v, want must be true or false", v, err)
		}
	}
}

// Boundary: "True", "TRUE" and "1" display true in the read model.
func TestStoredBoolOverrideDisplaysParsedValue(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"True", true}, {"TRUE", true}, {"1", true}, {"0", false}, {"False", false}} {
		m := map[string]any{"advisor.enabled": map[string]any{
			"value": !tc.want, "source": "yaml"}}
		applyOverrides(m, []ConfigOverride{{Key: "advisor.enabled", Value: tc.value}},
			"override")
		entry := m["advisor.enabled"].(map[string]any)
		if entry["value"] != tc.want || entry["source"] != "override" {
			t.Errorf("stored %q displays %+v, want value %v from override",
				tc.value, entry, tc.want)
		}
	}
}

// The read model shows the value the runtime replays: an invalid stored row
// does not displace the effective value (global and per database alike).
func TestApplyOverridesSkipsRowsTheReplaySkips(t *testing.T) {
	m := map[string]any{"collector.interval_seconds": map[string]any{
		"value": 60, "source": "yaml"}}
	applyOverrides(m, []ConfigOverride{
		{Key: "collector.interval_seconds", Value: "abc", DatabaseID: 3},
	}, "db_override")
	entry := m["collector.interval_seconds"].(map[string]any)
	if entry["value"] != 60 || entry["source"] != "yaml" {
		t.Fatalf("invalid stored row displayed: %+v, want 60 from yaml", entry)
	}
}

func TestReplayOverrideDecisions(t *testing.T) {
	for _, tc := range []struct {
		key, value, apply string
		ok                bool
		problem           string
	}{
		{"collector.interval_seconds", "120", "120", true, ""},
		{"collector.interval_seconds", "abc", "", false, "int_min5"},
		{"collector.interval_seconds", "4", "", false, "int_min5"},
		{"analyzer.unused_index_window_days", "0", "", false, "int_pos"},
		{"tuner.llm_enabled", "false", "", false, "not a settable"},
		{"", "x", "", false, ""},
		{"emergency_stop", "true", "", false, ""},
		{"trust_ramp_start", "2026-01-01T00:00:00Z", "", false, ""},
		{"llm.api_key_old", "sk-x", "", false, "not a settable"},
		{"llm.enabled", "TRUE", "TRUE", true, ""},
		{"llm.enabled", "yes", "", false, "bool"},
		{"trust.maintenance_window", "sometimes", "never", true, "stays closed"},
		{"trust.maintenance_window", "never", "never", true, ""},
		{"trust.maintenance_window", "", "", true, ""},
	} {
		apply, ok, problem := ReplayOverride(tc.key, tc.value)
		if apply != tc.apply || ok != tc.ok {
			t.Errorf("ReplayOverride(%q, %q) = (%q, %v), want (%q, %v)",
				tc.key, tc.value, apply, ok, tc.apply, tc.ok)
		}
		if tc.problem == "" && problem != "" {
			t.Errorf("ReplayOverride(%q, %q) problem %q, want none", tc.key, tc.value, problem)
		}
		if tc.problem != "" && !strings.Contains(problem, tc.problem) {
			t.Errorf("ReplayOverride(%q, %q) problem %q, want it to mention %q",
				tc.key, tc.value, problem, tc.problem)
		}
		if tc.value != "" && strings.Contains(problem, tc.value) {
			t.Errorf("problem %q repeats the stored value %q", problem, tc.value)
		}
	}
}

// The warning names the key, the scope and updated_at, never the value: a
// stored value may be a credential.
func TestStoredOverrideWarningOmitsTheValue(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	secret := "sk-live-should-never-be-logged"
	override := ConfigOverride{Key: "llm.api_key_old", Value: secret, UpdatedAt: at}
	_, _, problem := ReplayOverride(override.Key, override.Value)
	msg := StoredOverrideWarning(override, problem)
	for _, want := range []string{"llm.api_key_old", "global", "2026-03-04T05:06:07Z",
		"DELETE /api/v1/config/global/llm.api_key_old"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("warning leaks the stored value: %q", msg)
	}
	override.DatabaseID = 4
	msg = StoredOverrideWarning(override, problem)
	if !strings.Contains(msg, "database 4") ||
		!strings.Contains(msg, "/api/v1/config/databases/4/llm.api_key_old") {
		t.Fatalf("per-database warning %q does not name database 4 and its path", msg)
	}
}
