package config

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestApplyHotReload_ChangesCollectorInterval(t *testing.T) {
	target := &Config{
		Collector: CollectorConfig{IntervalSeconds: 60},
	}
	fresh := &Config{
		Collector: CollectorConfig{IntervalSeconds: 120},
	}
	changed := applyHotReload(target, fresh)
	if target.Collector.IntervalSeconds != 120 {
		t.Errorf("interval = %d, want 120", target.Collector.IntervalSeconds)
	}
	if len(changed) != 1 || changed[0] != "collector.interval_seconds" {
		t.Errorf("changed = %v, want [collector.interval_seconds]", changed)
	}
}

func TestApplyHotReload_IgnoresZeroValues(t *testing.T) {
	target := &Config{
		Collector: CollectorConfig{IntervalSeconds: 60, BatchSize: 500},
	}
	fresh := &Config{
		Collector: CollectorConfig{IntervalSeconds: 0, BatchSize: 0},
	}
	changed := applyHotReload(target, fresh)
	if len(changed) != 0 {
		t.Errorf("expected no changes, got %v", changed)
	}
	if target.Collector.IntervalSeconds != 60 {
		t.Errorf("interval should be unchanged, got %d", target.Collector.IntervalSeconds)
	}
}

func TestApplyHotReload_TrustLevel(t *testing.T) {
	target := &Config{Trust: TrustConfig{Level: "observation"}}
	fresh := &Config{Trust: TrustConfig{Level: "advisory"}}
	changed := applyHotReload(target, fresh)
	if target.Trust.Level != "advisory" {
		t.Errorf("trust = %q, want advisory", target.Trust.Level)
	}
	found := false
	for _, c := range changed {
		if c == "trust.level" {
			found = true
		}
	}
	if !found {
		t.Error("trust.level not in changed list")
	}
}

func TestWarnNonReloadable_LogsWarnings(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	current := &Config{
		Mode:       "standalone",
		Postgres:   PostgresConfig{Host: "localhost", Port: 5432},
		Prometheus: PrometheusConfig{ListenAddr: ":9187"},
	}
	fresh := &Config{
		Mode:       "fleet",
		Postgres:   PostgresConfig{Host: "db.prod.internal", Port: 5433},
		Prometheus: PrometheusConfig{ListenAddr: ":9188"},
	}
	warnNonReloadable(current, fresh)

	output := buf.String()
	expectations := []string{
		"mode changed",
		"postgres.host changed",
		"postgres.port changed",
		"prometheus.listen_addr changed",
	}
	for _, exp := range expectations {
		if !strings.Contains(output, exp) {
			t.Errorf("expected warning containing %q, log output:\n%s", exp, output)
		}
	}
}

func TestWarnNonReloadable_NoWarningsWhenUnchanged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	cfg := &Config{
		Mode:       "standalone",
		Postgres:   PostgresConfig{Host: "localhost", Port: 5432},
		Prometheus: PrometheusConfig{ListenAddr: ":9187"},
	}
	warnNonReloadable(cfg, cfg)

	if buf.Len() > 0 {
		t.Errorf("expected no warnings, got: %s", buf.String())
	}
}
