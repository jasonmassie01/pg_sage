package config

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// config-08: a SAGE_* value that is set but cannot be parsed stops Load,
// naming every such variable; an explicit zero is a value, not "unset";
// SAGE_PROMETHEUS_PORT keeps the configured host.
//
// No concurrent-access tests: overlayEnv runs once inside Load, before any
// goroutine of the process starts, and shares no state between calls.

// isolateEnvLoad gives Load a clean process: no SAGE_* variable from the
// developer's shell, no config.yaml auto-detected from the package dir.
func isolateEnvLoad(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name := strings.SplitN(kv, "=", 2)[0]
		if strings.HasPrefix(name, "SAGE_") {
			t.Setenv(name, "")
		}
	}
	t.Chdir(t.TempDir())
	t.Setenv("SAGE_DATABASE_URL", "postgres://sage@localhost:5432/app?sslmode=disable")
}

func writeEnvYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadEnvOK(t *testing.T, args ...string) *Config {
	t.Helper()
	cfg, err := Load(args)
	if err != nil {
		t.Fatalf("Load(%v): %v", args, err)
	}
	return cfg
}

func loadEnvErr(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := Load(args); err != nil {
		return err.Error()
	}
	t.Fatalf("Load(%v) succeeded, want an error", args)
	return ""
}

func TestLoadRejectsEveryUnparseableEnvInOneError(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_MCP_ENABLED", "no")
	t.Setenv("SAGE_PG_PORT", "abc")
	msg := loadEnvErr(t)
	for _, want := range []string{
		`SAGE_MCP_ENABLED="no": not a boolean`,
		`SAGE_PG_PORT="abc": not an integer`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Load error = %q, want it to contain %q", msg, want)
		}
	}
}

func TestLoadRejectsUnparseableEnvOfEveryKind(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"SAGE_PG_MAX_CONNS", "two", "not an integer"},
		{"SAGE_PG_PORT", "5432.0", "not an integer"},
		{"SAGE_PG_PORT", " 5433", "not an integer"},
		{"SAGE_VERIFY_MIN_GAIN_PCT", "abc", "not a number"},
		{"SAGE_VERIFY_MIN_GAIN_PCT", "NaN", "not a finite number"},
		{"SAGE_VERIFY_REGRESS_PCT", "+Inf", "not a finite number"},
		{"SAGE_VERIFY_WINDOW_MINUTES", "2h", "not an integer"},
		{"SAGE_CUSTODIAN_WAL_DISK_PCT_CEILING", "10%", "not a number"},
		{"SAGE_CLOUD_TELEMETRY_ENABLED", "yes", "not a boolean"},
		{"SAGE_MCP_ENABLED", "off", "not a boolean"},
		{"SAGE_RATE_LIMIT", "lots", "not an integer"},
		{"SAGE_PROMETHEUS_PORT", "abc", "not a port"},
		{"SAGE_PROMETHEUS_PORT", "0", "not a port"},
		{"SAGE_PROMETHEUS_PORT", "65536", "not a port"},
		{"SAGE_PROMETHEUS_PORT", "-1", "not a port"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			isolateEnvLoad(t)
			t.Setenv(tc.name, tc.value)
			msg := loadEnvErr(t)
			want := tc.name + "=" + strconv.Quote(tc.value) + ": " + tc.want
			if !strings.Contains(msg, want) {
				t.Fatalf("Load error = %q, want it to contain %q", msg, want)
			}
		})
	}
}

// Error propagation: the exact wording the operator sees, and the
// variables listed in the order overlayEnv reads them.
func TestOverlayEnvErrorNamesEachVariableInOrder(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PG_PORT", "abc")
	t.Setenv("SAGE_VERIFY_MIN_GAIN_PCT", "lots")
	t.Setenv("SAGE_MCP_ENABLED", "no")
	err := overlayEnv(newDefaults())
	if err == nil {
		t.Fatal("overlayEnv accepted three unparseable variables")
	}
	msg := err.Error()
	first := strings.Index(msg, `SAGE_PG_PORT="abc": not an integer`)
	second := strings.Index(msg, `SAGE_VERIFY_MIN_GAIN_PCT="lots": not a number`)
	third := strings.Index(msg, `SAGE_MCP_ENABLED="no": not a boolean`)
	if first < 0 || second < 0 || third < 0 || !(first < second && second < third) {
		t.Fatalf("overlayEnv error = %q, want all three in read order", msg)
	}
	if !strings.Contains(msg, "fix or unset") {
		t.Fatalf("overlayEnv error = %q, want it to say how to fix it", msg)
	}
}

// The valid variables are still applied when another one fails, so the
// error lists only the variables that are actually wrong.
func TestOverlayEnvAppliesValidValuesBesideInvalidOnes(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PG_PORT", "abc")
	t.Setenv("SAGE_PG_MAX_CONNS", "9")
	t.Setenv("SAGE_PG_HOST", "db.internal")
	cfg := newDefaults()
	err := overlayEnv(cfg)
	if err == nil || strings.Contains(err.Error(), "SAGE_PG_MAX_CONNS") {
		t.Fatalf("overlayEnv error = %v, want only SAGE_PG_PORT", err)
	}
	if cfg.Postgres.MaxConnections != 9 || cfg.Postgres.Host != "db.internal" {
		t.Fatalf("valid values not applied: max_conns=%d host=%q",
			cfg.Postgres.MaxConnections, cfg.Postgres.Host)
	}
	if cfg.Postgres.Port != DefaultPGPort {
		t.Fatalf("Postgres.Port = %d after an unparseable value, want %d",
			cfg.Postgres.Port, DefaultPGPort)
	}
}

func TestEnvBooleansAcceptStrconvSpellings(t *testing.T) {
	for value, want := range map[string]bool{
		"false": false, "0": false, "FALSE": false, "true": true, "1": true, "T": true,
	} {
		t.Run(value, func(t *testing.T) {
			isolateEnvLoad(t)
			t.Setenv("SAGE_MCP_ENABLED", value)
			t.Setenv("SAGE_CLOUD_TELEMETRY_ENABLED", value)
			cfg := loadEnvOK(t)
			if cfg.MCP.Enabled != want {
				t.Errorf("MCP.Enabled = %t, want %t", cfg.MCP.Enabled, want)
			}
			got := cfg.CloudTelemetry.Enabled
			if got == nil || *got != want {
				t.Errorf("CloudTelemetry.Enabled = %v, want %t", got, want)
			}
		})
	}
}

// Boundary: an explicit zero is applied, then range validation decides.
func TestEnvExplicitZeroIsApplied(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_VERIFY_MIN_GAIN_PCT", "0")
	t.Setenv("SAGE_VERIFY_REGRESS_PCT", "0")
	t.Setenv("SAGE_VERIFY_WRITE_IMPACT_PCT", "0.0")
	cfg := loadEnvOK(t)
	if cfg.Verify.MinGainPct != 0 || cfg.Verify.RegressPct != 0 ||
		cfg.Verify.WriteImpactPct != 0 {
		t.Fatalf("verify pcts = %v/%v/%v, want 0/0/0", cfg.Verify.MinGainPct,
			cfg.Verify.RegressPct, cfg.Verify.WriteImpactPct)
	}
}

func TestEnvZeroIsRejectedByRangeValidation(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"SAGE_PG_PORT", "0", "postgres.port"},
		{"SAGE_PG_PORT", "65536", "postgres.port"},
		{"SAGE_PG_PORT", "-1", "postgres.port"},
		{"SAGE_VERIFY_WINDOW_MINUTES", "0", "verify window"},
		{"SAGE_VERIFY_MIN_SAMPLES", "0", "min_samples"},
		{"SAGE_VERIFY_MIN_GAIN_PCT", "-1", "percentages"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			isolateEnvLoad(t)
			t.Setenv(tc.name, tc.value)
			msg := loadEnvErr(t)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("Load error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

func TestEnvPortBoundsAccepted(t *testing.T) {
	for _, port := range []int{1, 65535} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			isolateEnvLoad(t)
			t.Setenv("SAGE_PG_PORT", strconv.Itoa(port))
			t.Setenv("SAGE_PROMETHEUS_PORT", strconv.Itoa(port))
			cfg := loadEnvOK(t)
			if cfg.Postgres.Port != port {
				t.Errorf("Postgres.Port = %d, want %d", cfg.Postgres.Port, port)
			}
			want := "127.0.0.1:" + strconv.Itoa(port)
			if cfg.Prometheus.ListenAddr != want {
				t.Errorf("Prometheus.ListenAddr = %q, want %q",
					cfg.Prometheus.ListenAddr, want)
			}
		})
	}
}

func TestPrometheusPortKeepsTheDefaultLoopbackHost(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PROMETHEUS_PORT", "9188")
	cfg := loadEnvOK(t)
	if cfg.Prometheus.ListenAddr != "127.0.0.1:9188" {
		t.Fatalf("Prometheus.ListenAddr = %q, want 127.0.0.1:9188",
			cfg.Prometheus.ListenAddr)
	}
}

func TestPrometheusPortKeepsTheYAMLHost(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:9187": "0.0.0.0:9188",
		"[::1]:9187":   "[::1]:9188",
		"10.0.0.5:80":  "10.0.0.5:9188",
		":9187":        ":9188",
	}
	for yamlAddr, want := range cases {
		t.Run(yamlAddr, func(t *testing.T) {
			isolateEnvLoad(t)
			path := writeEnvYAML(t, "prometheus:\n  listen_addr: \""+yamlAddr+"\"\n")
			t.Setenv("SAGE_PROMETHEUS_PORT", "9188")
			cfg := loadEnvOK(t, "--config", path)
			if cfg.Prometheus.ListenAddr != want {
				t.Fatalf("Prometheus.ListenAddr = %q, want %q",
					cfg.Prometheus.ListenAddr, want)
			}
		})
	}
}

// A listen address with no port cannot be split; the port then pairs with
// the loopback default host, never with every interface.
func TestPrometheusPortWithUnsplittableAddrUsesLoopback(t *testing.T) {
	cfg := newDefaults()
	cfg.Prometheus.ListenAddr = "metrics-host"
	t.Setenv("SAGE_PROMETHEUS_PORT", "9188")
	if err := overlayEnv(cfg); err != nil {
		t.Fatalf("overlayEnv: %v", err)
	}
	if cfg.Prometheus.ListenAddr != "127.0.0.1:9188" {
		t.Fatalf("Prometheus.ListenAddr = %q, want 127.0.0.1:9188",
			cfg.Prometheus.ListenAddr)
	}
}

func TestPromAddrFlagStillBeatsPrometheusPortEnv(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_PROMETHEUS_PORT", "9188")
	cfg := loadEnvOK(t, "--prom-addr", "0.0.0.0:9300")
	if cfg.Prometheus.ListenAddr != "0.0.0.0:9300" {
		t.Fatalf("Prometheus.ListenAddr = %q, want the flag's 0.0.0.0:9300",
			cfg.Prometheus.ListenAddr)
	}
}

// Nil/empty: with no config file, unset and empty variables leave every
// default in place.
func TestEnvUnsetOrEmptyKeepsDefaults(t *testing.T) {
	for _, value := range []string{"unset", ""} {
		t.Run("value="+value, func(t *testing.T) {
			isolateEnvLoad(t)
			if value == "" {
				for _, name := range []string{"SAGE_PG_PORT", "SAGE_MCP_ENABLED",
					"SAGE_PROMETHEUS_PORT", "SAGE_VERIFY_MIN_GAIN_PCT",
					"SAGE_CLOUD_TELEMETRY_ENABLED", "SAGE_RATE_LIMIT"} {
					t.Setenv(name, "")
				}
			}
			cfg := loadEnvOK(t)
			def := newDefaults()
			if cfg.Postgres.Port != DefaultPGPort ||
				cfg.Postgres.MaxConnections != DefaultPGMaxConnections {
				t.Errorf("postgres = %d/%d, want defaults", cfg.Postgres.Port,
					cfg.Postgres.MaxConnections)
			}
			if !cfg.MCP.Enabled || cfg.Prometheus.ListenAddr != DefaultPrometheusListenAddr {
				t.Errorf("mcp=%t prom=%q, want defaults", cfg.MCP.Enabled,
					cfg.Prometheus.ListenAddr)
			}
			if cfg.Verify != def.Verify {
				t.Errorf("verify = %+v, want defaults %+v", cfg.Verify, def.Verify)
			}
			if cfg.CloudTelemetry.Enabled != nil {
				t.Errorf("cloud telemetry enabled = %v, want unset", *cfg.CloudTelemetry.Enabled)
			}
		})
	}
}

func TestEnvEmptyKeepsTheYAMLValue(t *testing.T) {
	isolateEnvLoad(t)
	path := writeEnvYAML(t, "postgres:\n  port: 6543\nmcp:\n  enabled: false\n"+
		"verify:\n  min_gain_pct: 33\n")
	t.Setenv("SAGE_PG_PORT", "")
	t.Setenv("SAGE_MCP_ENABLED", "")
	t.Setenv("SAGE_VERIFY_MIN_GAIN_PCT", "")
	cfg := loadEnvOK(t, "--config", path)
	if cfg.Postgres.Port != 6543 || cfg.MCP.Enabled || cfg.Verify.MinGainPct != 33 {
		t.Fatalf("port=%d mcp=%t gain=%v, want the YAML 6543/false/33",
			cfg.Postgres.Port, cfg.MCP.Enabled, cfg.Verify.MinGainPct)
	}
}

// Precedence is unchanged: CLI > env > YAML > defaults.
func TestEnvPrecedenceIsUnchanged(t *testing.T) {
	isolateEnvLoad(t)
	path := writeEnvYAML(t, "postgres:\n  port: 6543\n")
	t.Setenv("SAGE_PG_PORT", "7000")
	if cfg := loadEnvOK(t, "--config", path); cfg.Postgres.Port != 7000 {
		t.Fatalf("env did not beat YAML: port=%d", cfg.Postgres.Port)
	}
	if cfg := loadEnvOK(t, "--config", path, "--pg-port", "7100"); cfg.Postgres.Port != 7100 {
		t.Fatalf("flag did not beat env: port=%d", cfg.Postgres.Port)
	}
}

func TestLookupEnvReportsSetAndParseErrors(t *testing.T) {
	t.Setenv("SAGE_TEST_LOOKUP", "")
	v, ok, err := lookupEnv("SAGE_TEST_LOOKUP", parseEnvInt)
	if v != 0 || ok || err != nil {
		t.Fatalf("empty: (%d, %t, %v), want (0, false, nil)", v, ok, err)
	}
	t.Setenv("SAGE_TEST_LOOKUP", "-5")
	v, ok, err = lookupEnv("SAGE_TEST_LOOKUP", parseEnvInt)
	if v != -5 || !ok || err != nil {
		t.Fatalf("valid: (%d, %t, %v), want (-5, true, nil)", v, ok, err)
	}
	t.Setenv("SAGE_TEST_LOOKUP", "x")
	v, ok, err = lookupEnv("SAGE_TEST_LOOKUP", parseEnvInt)
	if v != 0 || ok || err == nil ||
		err.Error() != `SAGE_TEST_LOOKUP="x": not an integer` {
		t.Fatalf("invalid: (%d, %t, %v), want a named error", v, ok, err)
	}
}

func TestParseEnvFloatRejectsNonFinite(t *testing.T) {
	for _, raw := range []string{"NaN", "Inf", "-Inf", "1e999"} {
		if f, err := parseEnvFloat(raw); err == nil {
			t.Errorf("parseEnvFloat(%q) = %v, want an error", raw, f)
		}
	}
	if f, err := parseEnvFloat("-2.5"); err != nil || f != -2.5 {
		t.Fatalf("parseEnvFloat(-2.5) = %v, %v", f, err)
	}
	if f, err := parseEnvFloat("42"); err != nil || f != 42 || math.IsNaN(f) {
		t.Fatalf("parseEnvFloat(42) = %v, %v", f, err)
	}
}

// RateLimit reads SAGE_RATE_LIMIT at runtime; Load has already refused a
// junk value, and a non-positive one still means the default.
func TestRateLimitEnvIsCheckedAtLoadAndReadAtRuntime(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_RATE_LIMIT", "120")
	cfg := loadEnvOK(t)
	if got := cfg.RateLimit(); got != 120 {
		t.Fatalf("RateLimit() = %d, want 120", got)
	}
	t.Setenv("SAGE_RATE_LIMIT", "0")
	if got := loadEnvOK(t).RateLimit(); got != DefaultRateLimit {
		t.Fatalf("RateLimit() with 0 = %d, want default %d", got, DefaultRateLimit)
	}
}

// State transition across modes: only standalone mode connects with the
// postgres section, so only there does its port have to be usable.
func TestPostgresPortValidationAppliesOnlyToStandalone(t *testing.T) {
	cfg := newDefaults()
	cfg.Postgres.Port = 0
	cfg.Mode = "fleet"
	if err := cfg.validatePostgresPort(); err != nil {
		t.Fatalf("fleet mode rejected an unused postgres.port: %v", err)
	}
	cfg.Mode, cfg.MetaDB = "standalone", "postgres://sage@meta/sage_meta"
	if err := cfg.validatePostgresPort(); err != nil {
		t.Fatalf("meta-db standalone rejected an unused postgres.port: %v", err)
	}
	cfg.MetaDB = ""
	err := cfg.validatePostgresPort()
	const wantRange = "postgres.port must be from 1 to 65535, got 0"
	if err == nil || !strings.Contains(err.Error(), wantRange) ||
		!strings.Contains(err.Error(), "--meta-db") {
		t.Fatalf("standalone port 0 error = %v, want a range error naming --meta-db", err)
	}
	cfg.Postgres.Port = DefaultPGPort
	if err := cfg.validatePostgresPort(); err != nil {
		t.Fatalf("default port rejected: %v", err)
	}
}
