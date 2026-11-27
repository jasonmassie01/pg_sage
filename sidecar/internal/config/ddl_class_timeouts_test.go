package config

import (
	"strings"
	"testing"
	"time"
)

// executor-05: each class timeout defaults to four hours, is bounded to
// 60-86400 seconds, and is never unlimited.

func TestClassTimeoutDefaultsWithoutAnyConfig(t *testing.T) {
	cfg := DefaultConfig()
	for name, got := range map[string]int{
		"index_build": cfg.Safety.IndexBuildTimeoutSeconds,
		"reindex":     cfg.Safety.ReindexTimeoutSeconds,
		"vacuum":      cfg.Safety.VacuumTimeoutSeconds,
	} {
		if got != 14400 {
			t.Errorf("default %s timeout = %d, want 14400", name, got)
		}
	}
	if cfg.Safety.DDLTimeoutSeconds != 300 {
		t.Errorf("ddl_timeout_seconds default = %d, want it unchanged at 300",
			cfg.Safety.DDLTimeoutSeconds)
	}
}

func TestClassTimeoutZeroValueIsNeverUnlimited(t *testing.T) {
	var s SafetyConfig // zero-value struct: no default applied
	want := 4 * time.Hour
	if s.IndexBuildTimeout() != want || s.ReindexTimeout() != want ||
		s.VacuumTimeout() != want {
		t.Fatalf("zero class timeouts = %s/%s/%s, want %s each", s.IndexBuildTimeout(),
			s.ReindexTimeout(), s.VacuumTimeout(), want)
	}
	s.IndexBuildTimeoutSeconds, s.ReindexTimeoutSeconds, s.VacuumTimeoutSeconds = 600, 700, -5
	if s.IndexBuildTimeout() != 10*time.Minute || s.ReindexTimeout() != 700*time.Second ||
		s.VacuumTimeout() != want {
		t.Fatalf("configured class timeouts = %s/%s/%s", s.IndexBuildTimeout(),
			s.ReindexTimeout(), s.VacuumTimeout())
	}
}

func TestClassTimeoutBoundaries(t *testing.T) {
	keys := []struct {
		key string
		set func(*SafetyConfig, int)
	}{
		{"safety.index_build_timeout_seconds",
			func(s *SafetyConfig, v int) { s.IndexBuildTimeoutSeconds = v }},
		{"safety.reindex_timeout_seconds",
			func(s *SafetyConfig, v int) { s.ReindexTimeoutSeconds = v }},
		{"safety.vacuum_timeout_seconds",
			func(s *SafetyConfig, v int) { s.VacuumTimeoutSeconds = v }},
	}
	cases := []struct {
		seconds int
		ok      bool
	}{{-1, false}, {0, true}, {1, false}, {59, false}, {60, true}, {86400, true},
		{86401, false}}
	for _, k := range keys {
		for _, tc := range cases {
			cfg := DefaultConfig()
			k.set(&cfg.Safety, tc.seconds)
			err := cfg.validate()
			if tc.ok && err != nil {
				t.Errorf("%s=%d: unexpected error %v", k.key, tc.seconds, err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), k.key) ||
				!strings.Contains(err.Error(), "60-86400")) {
				t.Errorf("%s=%d: error %v, want one naming the key and 60-86400",
					k.key, tc.seconds, err)
			}
		}
	}
}

func TestClassTimeoutsLoadFromYAML(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML+`
safety:
  index_build_timeout_seconds: 7200
  reindex_timeout_seconds: 3600
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Safety.IndexBuildTimeoutSeconds != 7200 || cfg.Safety.ReindexTimeoutSeconds != 3600 {
		t.Fatalf("loaded index/reindex = %d/%d, want 7200/3600",
			cfg.Safety.IndexBuildTimeoutSeconds, cfg.Safety.ReindexTimeoutSeconds)
	}
	if cfg.Safety.VacuumTimeoutSeconds != 14400 {
		t.Fatalf("unset vacuum timeout = %d, want the 14400 default",
			cfg.Safety.VacuumTimeoutSeconds)
	}
	if _, err := loadYAMLResult(t, standaloneIOYAML+`
safety:
  vacuum_timeout_seconds: 90000
`); err == nil || !strings.Contains(err.Error(), "safety.vacuum_timeout_seconds") {
		t.Fatalf("load of 90000 s = %v, want a range error", err)
	}
}
