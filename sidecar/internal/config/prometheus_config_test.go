package config

import (
	"strings"
	"testing"
)

// observability-08: prometheus.serve_on_api moves /metrics onto the API
// listener; an empty prometheus.listen_addr opens no separate listener;
// SAGE_PROMETHEUS_ADDR sets the whole address and wins over
// SAGE_PROMETHEUS_PORT.
//
// No concurrent-access tests: Load runs once, before any goroutine of the
// process starts, and shares no state between calls.

// Nil/empty: no config file keeps today's listener and no API route.
func TestPrometheusConfigDefaultsKeepTheSeparateListener(t *testing.T) {
	isolateEnvLoad(t)
	cfg := loadEnvOK(t)
	if cfg.Prometheus.ServeOnAPI {
		t.Error("prometheus.serve_on_api defaults to true, want false")
	}
	if cfg.Prometheus.ListenAddr != "127.0.0.1:8187" {
		t.Errorf("prometheus.listen_addr = %q, want the default 127.0.0.1:8187",
			cfg.Prometheus.ListenAddr)
	}
}

func TestPrometheusConfigServeOnAPIAndEmptyListenAddrLoad(t *testing.T) {
	isolateEnvLoad(t)
	path := writeEnvYAML(t, "prometheus:\n  serve_on_api: true\n  listen_addr: \"\"\n")
	cfg := loadEnvOK(t, "--config", path)
	if !cfg.Prometheus.ServeOnAPI {
		t.Error("serve_on_api: true did not load")
	}
	if cfg.Prometheus.ListenAddr != "" {
		t.Errorf("listen_addr: \"\" loaded as %q, want empty (no separate listener)",
			cfg.Prometheus.ListenAddr)
	}
}

// Boundary: SAGE_PROMETHEUS_ADDR replaces the YAML address, host included.
func TestPrometheusConfigAddrEnvOverridesYAML(t *testing.T) {
	isolateEnvLoad(t)
	path := writeEnvYAML(t, "prometheus:\n  listen_addr: \"0.0.0.0:9187\"\n")
	t.Setenv("SAGE_PROMETHEUS_ADDR", "127.0.0.1:9300")
	cfg := loadEnvOK(t, "--config", path)
	if cfg.Prometheus.ListenAddr != "127.0.0.1:9300" {
		t.Fatalf("prometheus.listen_addr = %q, want SAGE_PROMETHEUS_ADDR's 127.0.0.1:9300",
			cfg.Prometheus.ListenAddr)
	}
}

func TestPrometheusConfigAddrEnvWinsOverPortEnv(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PROMETHEUS_PORT", "9188")
	t.Setenv("SAGE_PROMETHEUS_ADDR", "127.0.0.1:9300")
	cfg := loadEnvOK(t)
	if cfg.Prometheus.ListenAddr != "127.0.0.1:9300" {
		t.Fatalf("prometheus.listen_addr = %q, want SAGE_PROMETHEUS_ADDR to win: "+
			"127.0.0.1:9300", cfg.Prometheus.ListenAddr)
	}
}

// The address variable re-enables a listener the YAML disabled: an explicit
// environment setting is the operator's latest word.
func TestPrometheusConfigAddrEnvReenablesAnEmptyYAMLAddr(t *testing.T) {
	isolateEnvLoad(t)
	path := writeEnvYAML(t, "prometheus:\n  listen_addr: \"\"\n")
	t.Setenv("SAGE_PROMETHEUS_ADDR", "[::1]:9301")
	cfg := loadEnvOK(t, "--config", path)
	if cfg.Prometheus.ListenAddr != "[::1]:9301" {
		t.Fatalf("prometheus.listen_addr = %q, want [::1]:9301", cfg.Prometheus.ListenAddr)
	}
}

func TestPrometheusConfigFlagBeatsAddrEnv(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PROMETHEUS_ADDR", "127.0.0.1:9300")
	cfg := loadEnvOK(t, "--prom-addr", "127.0.0.1:9400")
	if cfg.Prometheus.ListenAddr != "127.0.0.1:9400" {
		t.Fatalf("prometheus.listen_addr = %q, want the flag's 127.0.0.1:9400",
			cfg.Prometheus.ListenAddr)
	}
}

// Nil/empty: an empty SAGE_PROMETHEUS_ADDR is unset, like every SAGE_* value.
func TestPrometheusConfigEmptyAddrEnvIsUnset(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PROMETHEUS_ADDR", "")
	cfg := loadEnvOK(t)
	if cfg.Prometheus.ListenAddr != DefaultPrometheusListenAddr {
		t.Fatalf("prometheus.listen_addr = %q, want the default", cfg.Prometheus.ListenAddr)
	}
}

// Invalid input: a malformed address stops Load and names the variable.
func TestPrometheusConfigRejectsMalformedAddrEnv(t *testing.T) {
	for _, raw := range []string{"9300", "localhost", "127.0.0.1:", "127.0.0.1:abc",
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:-1", "a:b:c"} {
		t.Run(raw, func(t *testing.T) {
			isolateEnvLoad(t)
			t.Setenv("SAGE_PROMETHEUS_ADDR", raw)
			msg := loadEnvErr(t)
			if !strings.Contains(msg, "SAGE_PROMETHEUS_ADDR") ||
				!strings.Contains(msg, "host:port") {
				t.Fatalf("Load error = %q, want it to name SAGE_PROMETHEUS_ADDR and "+
					"host:port", msg)
			}
		})
	}
}

// Boundary: ports 1 and 65535 are accepted.
func TestPrometheusConfigAddrEnvPortBounds(t *testing.T) {
	for _, raw := range []string{"127.0.0.1:1", "127.0.0.1:65535", ":9300"} {
		t.Run(raw, func(t *testing.T) {
			isolateEnvLoad(t)
			t.Setenv("SAGE_PROMETHEUS_ADDR", raw)
			if cfg := loadEnvOK(t); cfg.Prometheus.ListenAddr != raw {
				t.Fatalf("prometheus.listen_addr = %q, want %q", cfg.Prometheus.ListenAddr, raw)
			}
		})
	}
}

// Invalid input: a YAML listen address that net.Listen could never bind is
// refused at load instead of failing quietly at startup.
func TestPrometheusConfigRejectsMalformedYAMLListenAddr(t *testing.T) {
	for _, addr := range []string{"metrics-host", "127.0.0.1:abc", "127.0.0.1:70000"} {
		t.Run(addr, func(t *testing.T) {
			err := PrometheusConfig{ListenAddr: addr}.validate()
			if err == nil || !strings.Contains(err.Error(), "prometheus.listen_addr") {
				t.Fatalf("validate(%q) = %v, want an error naming prometheus.listen_addr",
					addr, err)
			}
		})
	}
	for _, addr := range []string{"", ":9187", "0.0.0.0:9187", "[::1]:0", "localhost:9187"} {
		if err := (PrometheusConfig{ListenAddr: addr}).validate(); err != nil {
			t.Errorf("validate(%q) = %v, want nil", addr, err)
		}
	}
}

// State transition: both off is allowed (no metrics at all), and so is
// serving on both the API and the separate listener.
func TestPrometheusConfigListenerCombinations(t *testing.T) {
	for _, p := range []PrometheusConfig{
		{}, {ServeOnAPI: true}, {ListenAddr: "127.0.0.1:9187"},
		{ServeOnAPI: true, ListenAddr: "127.0.0.1:9187"},
	} {
		if err := p.validate(); err != nil {
			t.Errorf("validate(%+v) = %v, want nil", p, err)
		}
	}
	if (PrometheusConfig{}).SeparateListener() {
		t.Error("an empty listen_addr reports a separate listener")
	}
	if !(PrometheusConfig{ListenAddr: ":9187"}).SeparateListener() {
		t.Error("listen_addr :9187 reports no separate listener")
	}
}

func TestPrometheusConfigAddrEnvIsRegistered(t *testing.T) {
	for _, v := range EnvVars() {
		if v.Name == "SAGE_PROMETHEUS_ADDR" {
			if !strings.Contains(v.Doc, "SAGE_PROMETHEUS_PORT") {
				t.Errorf("SAGE_PROMETHEUS_ADDR doc %q does not say it wins over "+
					"SAGE_PROMETHEUS_PORT", v.Doc)
			}
			return
		}
	}
	t.Fatal("SAGE_PROMETHEUS_ADDR is not in the env registry")
}
