package config

import (
	"strings"
	"testing"
)

// debug.pprof_enabled: the Go profiler on the API listener, admin-only.
// Off unless set: a profile shows code paths and memory, so it is never
// on by default.

func TestDebugPprof_OffByDefault(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Debug.PprofEnabled || DefaultConfig().Debug.PprofEnabled {
		t.Fatal("pprof must be off by default")
	}
}

func TestDebugPprof_Enabled(t *testing.T) {
	cfg, err := loadRCAYAML(t, "debug:\n  pprof_enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Debug.PprofEnabled {
		t.Fatal("debug.pprof_enabled: true not loaded")
	}
}

func TestDebugPprof_UnknownKeyRefused(t *testing.T) {
	_, err := loadRCAYAML(t, "debug:\n  pprof_public: true\n")
	if err == nil || !strings.Contains(err.Error(), "pprof_public") {
		t.Fatalf("unknown debug key: err = %v, want a refusal", err)
	}
}
