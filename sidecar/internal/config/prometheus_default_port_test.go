package config

import (
	"net"
	"testing"
)

// observability-09 (v2.6.0, the rename release): the default metrics port
// moved off 9187, postgres_exporter's registered default, so running both
// exporters on one host no longer fails to bind.

// No concurrent access or state tests: these read constants and defaults.

func TestDefaultMetricsPortMovedOffPostgresExporters(t *testing.T) {
	host, port, err := net.SplitHostPort(DefaultPrometheusListenAddr)
	if err != nil {
		t.Fatalf("DefaultPrometheusListenAddr %q: %v", DefaultPrometheusListenAddr, err)
	}
	if host != "127.0.0.1" || port != "8187" {
		t.Fatalf("default listener = %s:%s, want 127.0.0.1:8187", host, port)
	}
	if got := DefaultConfig().Prometheus.ListenAddr; got != DefaultPrometheusListenAddr {
		t.Fatalf("DefaultConfig listen_addr = %q, want %q", got, DefaultPrometheusListenAddr)
	}
	for _, v := range EnvVars() {
		if v.Name == "SAGE_PROMETHEUS_PORT" {
			if v.Default != "`8187`" {
				t.Fatalf("SAGE_PROMETHEUS_PORT documented default = %s, want `8187`",
					v.Default)
			}
			return
		}
	}
	t.Fatal("SAGE_PROMETHEUS_PORT is not in the env registry")
}

// Boundary: an operator who keeps the old port says so explicitly, and
// SAGE_PROMETHEUS_PORT=9187 still gives the default host with that port.
func TestOldMetricsPortStillConfigurable(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PROMETHEUS_PORT", "9187")
	cfg := loadEnvOK(t)
	if cfg.Prometheus.ListenAddr != "127.0.0.1:9187" {
		t.Fatalf("SAGE_PROMETHEUS_PORT=9187 gives %q, want 127.0.0.1:9187",
			cfg.Prometheus.ListenAddr)
	}
}
