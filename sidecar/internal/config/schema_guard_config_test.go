package config

import (
	"strings"
	"testing"
	"time"
)

// analyzer.schema_guard_ddl_debounce_seconds: after pg_sage's own DDL the
// schema guard runs again at most once per this many seconds. Default 60;
// 0 means the default; out-of-range values are refused at load.

func TestSchemaGuardDDLDebounce_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.SchemaGuardDDLDebounceSeconds != 60 ||
		DefaultSchemaGuardDDLDebounceSeconds != 60 {
		t.Fatalf("default = %d (const %d), want 60",
			cfg.Analyzer.SchemaGuardDDLDebounceSeconds, DefaultSchemaGuardDDLDebounceSeconds)
	}
	if cfg.Analyzer.SchemaGuardDDLDebounce() != time.Minute {
		t.Fatalf("debounce = %v, want 1m", cfg.Analyzer.SchemaGuardDDLDebounce())
	}
	if DefaultConfig().Analyzer.SchemaGuardDDLDebounceSeconds != 60 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestSchemaGuardDDLDebounce_PartialSectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "analyzer:\n  interval_seconds: 120\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.SchemaGuardDDLDebounce() != time.Minute ||
		cfg.Analyzer.IntervalSeconds != 120 {
		t.Fatalf("analyzer = %+v, want the default debounce kept", cfg.Analyzer)
	}
}

func TestSchemaGuardDDLDebounce_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want time.Duration
	}{{"0", time.Minute}, {"1", time.Second}, {"300", 5 * time.Minute},
		{"3600", time.Hour}} {
		cfg, err := loadRCAYAML(t,
			"analyzer:\n  schema_guard_ddl_debounce_seconds: "+tc.yaml+"\n")
		if err != nil || cfg.Analyzer.SchemaGuardDDLDebounce() != tc.want {
			t.Errorf("%s: got %v (%v), want %v", tc.yaml, cfg, err, tc.want)
		}
	}
}

func TestSchemaGuardDDLDebounce_ZeroValueStructUsesDefault(t *testing.T) {
	var analyzer AnalyzerConfig
	if got := analyzer.SchemaGuardDDLDebounce(); got != time.Minute {
		t.Fatalf("zero-value debounce = %v, want the 1m default", got)
	}
}

func TestSchemaGuardDDLDebounce_OutOfRangeRefused(t *testing.T) {
	for _, v := range []string{"-1", "3601"} {
		_, err := loadRCAYAML(t, "analyzer:\n  schema_guard_ddl_debounce_seconds: "+v+"\n")
		if err == nil ||
			!strings.Contains(err.Error(), "analyzer.schema_guard_ddl_debounce_seconds") {
			t.Errorf("%s: err = %v, want a refusal naming the key", v, err)
		}
	}
}
