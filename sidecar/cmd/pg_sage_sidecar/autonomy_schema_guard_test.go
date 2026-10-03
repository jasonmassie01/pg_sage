package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// The schema guard takes its idle window for leftover clone families from
// analyzer.unused_index_window_days (the analyzer's own idle window) and its
// post-DDL debounce from analyzer.schema_guard_ddl_debounce_seconds.
func TestSchemaGuardRuntimeSettingsFollowConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	options := schemaGuardOptions(cfg)
	if options.IdleWindow != 7*24*time.Hour {
		t.Fatalf("default idle window = %v, want 7 days", options.IdleWindow)
	}
	if got := autonomyDDLDebounce(cfg); got != time.Minute {
		t.Fatalf("default debounce = %v, want 1m", got)
	}
	cfg.Analyzer.UnusedIndexWindowDays = 2
	cfg.Analyzer.SchemaGuardDDLDebounceSeconds = 15
	if got := schemaGuardOptions(cfg).IdleWindow; got != 48*time.Hour {
		t.Fatalf("idle window = %v, want 48h", got)
	}
	if got := autonomyDDLDebounce(cfg); got != 15*time.Second {
		t.Fatalf("debounce = %v, want 15s", got)
	}
	cfg.Analyzer.UnusedIndexWindowDays = 0
	if got := schemaGuardOptions(cfg).IdleWindow; got != 7*24*time.Hour {
		t.Fatalf("zero idle window = %v, want the 7-day default", got)
	}
}
