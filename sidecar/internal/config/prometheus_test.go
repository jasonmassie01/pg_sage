package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// observability-03: prometheus.refresh_interval_seconds defaults to 0
// (the refresher's 30 s) and rejects a negative value at load.
func TestPrometheusRefreshIntervalDefaultIsUnset(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Prometheus.RefreshIntervalSeconds != 0 {
		t.Fatalf("default = %d, want 0 (meaning %ds)",
			cfg.Prometheus.RefreshIntervalSeconds, DefaultPrometheusRefreshSeconds)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestPrometheusRefreshIntervalValidation(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		wantErr bool
	}{{-1, true}, {0, false}, {1, false}, {300, false}} {
		err := PrometheusConfig{RefreshIntervalSeconds: tc.seconds}.validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("seconds=%d: err = %v, wantErr %v", tc.seconds, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "prometheus.refresh_interval_seconds") {
			t.Errorf("error does not name the key: %v", err)
		}
	}
}

func TestPrometheusRefreshIntervalLoadsFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sage.yaml")
	body := "postgres:\n  host: localhost\nprometheus:\n  refresh_interval_seconds: 90\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("loadYAML: %v", err)
	}
	if cfg.Prometheus.RefreshIntervalSeconds != 90 ||
		cfg.Prometheus.ListenAddr != DefaultPrometheusListenAddr {
		t.Fatalf("prometheus = %+v, want refresh 90 and the default listener", cfg.Prometheus)
	}
	cfg.Prometheus.RefreshIntervalSeconds = -30
	if err := cfg.validate(); err == nil {
		t.Fatal("a negative refresh interval passed validation")
	}
}
