package config

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

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
