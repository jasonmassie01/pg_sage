package config

import (
	"testing"
	"time"
)

// config-03: Load records which keys the file layer (YAML, env, flags) set
// explicitly. It is a presence set: an explicit value equal to its default
// is still explicit, and a default-only value never is.
//
// No concurrent-access tests: the presence set is built inside one Load
// call and is never written again; Clone shares it read-only.

func TestLoadRecordsYAMLPresenceIncludingDefaultValues(t *testing.T) {
	isolateEnvLoad(t)
	path := writeEnvYAML(t, "trust:\n  level: observation\n"+
		"collector:\n  batch_size: 100\n")
	cfg := loadEnvOK(t, "--config", path)
	for _, key := range []string{"trust.level", "collector.batch_size"} {
		if !cfg.IsExplicit(key) {
			t.Errorf("IsExplicit(%q) = false, want true (set in the YAML)", key)
		}
	}
	if cfg.Trust.Level != DefaultTrustLevel {
		t.Fatalf("fixture: trust.level = %q, want the default %q", cfg.Trust.Level,
			DefaultTrustLevel)
	}
	for _, key := range []string{"trust.tier3_safe", "collector.interval_seconds",
		"trust", "collector"} {
		if cfg.IsExplicit(key) {
			t.Errorf("IsExplicit(%q) = true for a key the YAML does not set", key)
		}
	}
}

func TestLoadTreatsYAMLNullAsAbsent(t *testing.T) {
	isolateEnvLoad(t)
	cfg := loadEnvOK(t, "--config", writeEnvYAML(t, "trust:\n  level:\n"))
	if cfg.IsExplicit("trust.level") {
		t.Fatal("a null YAML value was recorded as explicit; it sets nothing")
	}
}

func TestLoadWithoutAnyInputHasNoExplicitPaths(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_DATABASE_URL", "")
	cfg := loadEnvOK(t)
	for _, key := range []string{"trust.level", "collector.batch_size", "mode"} {
		if cfg.IsExplicit(key) {
			t.Errorf("IsExplicit(%q) = true with no YAML, env or flag", key)
		}
	}
}

func TestLoadRecordsEnvPresenceEvenAtTheDefault(t *testing.T) {
	isolateEnvLoad(t)
	t.Setenv("SAGE_TRUST_LEVEL", "observation")
	t.Setenv("SAGE_MCP_ENABLED", "false")
	cfg := loadEnvOK(t)
	for _, key := range []string{"trust.level", "mcp.enabled", "postgres.database_url"} {
		if !cfg.IsExplicit(key) {
			t.Errorf("IsExplicit(%q) = false, want true (set by the environment)", key)
		}
	}
	if cfg.IsExplicit("llm.model") {
		t.Error("llm.model is explicit although SAGE_LLM_MODEL is empty")
	}
}

func TestLoadRecordsFlagPresence(t *testing.T) {
	isolateEnvLoad(t)
	cfg := loadEnvOK(t, "--pg-host", "db.internal", "--mode", "standalone")
	for _, key := range []string{"postgres.host", "mode"} {
		if !cfg.IsExplicit(key) {
			t.Errorf("IsExplicit(%q) = false, want true (set by a flag)", key)
		}
	}
	if cfg.IsExplicit("postgres.port") {
		t.Error("postgres.port is explicit although --pg-port was not given")
	}
}

func TestEveryEnvAndFlagPathIsAConfigField(t *testing.T) {
	for name, path := range envExplicitPaths {
		if _, ok := LookupFieldLifecycle(path); !ok {
			t.Errorf("env %s maps to %q, which is not a config field", name, path)
		}
	}
	for name, path := range flagExplicitPaths {
		if _, ok := LookupFieldLifecycle(path); !ok {
			t.Errorf("flag --%s maps to %q, which is not a config field", name, path)
		}
	}
}

func TestIsExplicitOnNilAndHandBuiltConfigs(t *testing.T) {
	var nilCfg *Config
	if nilCfg.IsExplicit("trust.level") {
		t.Error("nil config reports an explicit path")
	}
	if DefaultConfig().IsExplicit("trust.level") {
		t.Error("a hand-built default config reports an explicit path")
	}
	if got := nilCfg.MaskedByOverride(); got != nil {
		t.Errorf("nil config MaskedByOverride = %v, want nil", got)
	}
	if nilCfg.OverrideFileBase() != nil {
		t.Error("nil config has a file base")
	}
}

func TestValueAtPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Collector.BatchSize = 123
	cfg.Trust.Tier3Moderate = true
	if v, ok := ValueAtPath(cfg, "collector.batch_size"); !ok || v != 123 {
		t.Errorf("collector.batch_size = %v, %v; want 123, true", v, ok)
	}
	if v, ok := ValueAtPath(cfg, "trust.tier3_moderate"); !ok || v != true {
		t.Errorf("trust.tier3_moderate = %v, %v; want true, true", v, ok)
	}
	for _, path := range []string{"", "trust", "no.such.key", "trust.level.x"} {
		if _, ok := ValueAtPath(cfg, path); ok {
			t.Errorf("ValueAtPath(%q) ok, want false", path)
		}
	}
	if _, ok := ValueAtPath(nil, "trust.level"); ok {
		t.Error("ValueAtPath(nil) ok, want false")
	}
}

func TestSetOverrideMaskDetachesBaseAndCloneKeepsIt(t *testing.T) {
	base := DefaultConfig()
	base.Collector.BatchSize = 100
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	masked := []MaskedKey{{Key: "collector.batch_size", FileValue: "100",
		OverrideValue: "200", EffectiveValue: "200", UpdatedBy: 7, UpdatedAt: at}}
	cfg := DefaultConfig()
	cfg.SetOverrideMask(base, masked)
	base.Collector.BatchSize = 999
	masked[0].Key = "mutated"
	clone := Clone(cfg)
	for name, c := range map[string]*Config{"original": cfg, "clone": clone} {
		fileBase := c.OverrideFileBase()
		if fileBase == nil || fileBase.Collector.BatchSize != 100 {
			t.Fatalf("%s file base = %+v, want a detached batch_size 100", name, fileBase)
		}
		got := c.MaskedByOverride()
		if len(got) != 1 || got[0].Key != "collector.batch_size" ||
			got[0].UpdatedBy != 7 || !got[0].UpdatedAt.Equal(at) {
			t.Fatalf("%s masked = %+v, want the one detached entry", name, got)
		}
		if fileBase.OverrideFileBase() != nil {
			t.Fatalf("%s file base carries a nested file base", name)
		}
	}
	cfg.MaskedByOverride()[0].Key = "caller-mutated"
	if cfg.MaskedByOverride()[0].Key != "collector.batch_size" {
		t.Fatal("MaskedByOverride returned the internal slice")
	}
}

type restrictTrustCase struct {
	name, key       string
	file, override  func(*Config)
	check           func(*Config) bool
	wantRestrictive bool
}

func restrictTrustCases() []restrictTrustCase {
	return []restrictTrustCase{
		{"level lower in file", "trust.level",
			func(c *Config) { c.Trust.Level = "observation" },
			func(c *Config) { c.Trust.Level = "autonomous" },
			func(c *Config) bool { return c.Trust.Level == "observation" }, true},
		{"level lower in override", "trust.level",
			func(c *Config) { c.Trust.Level = "autonomous" },
			func(c *Config) { c.Trust.Level = "advisory" },
			func(c *Config) bool { return c.Trust.Level == "advisory" }, true},
		{"tier3_moderate file off", "trust.tier3_moderate",
			func(c *Config) { c.Trust.Tier3Moderate = false },
			func(c *Config) { c.Trust.Tier3Moderate = true },
			func(c *Config) bool { return !c.Trust.Tier3Moderate }, true},
		{"tier3_safe override off", "trust.tier3_safe",
			func(c *Config) { c.Trust.Tier3Safe = true },
			func(c *Config) { c.Trust.Tier3Safe = false },
			func(c *Config) bool { return !c.Trust.Tier3Safe }, true},
		{"tier3_high_risk file off", "trust.tier3_high_risk",
			func(c *Config) { c.Trust.Tier3HighRisk = false },
			func(c *Config) { c.Trust.Tier3HighRisk = true },
			func(c *Config) bool { return !c.Trust.Tier3HighRisk }, true},
		{"other key: override wins", "collector.batch_size",
			func(c *Config) { c.Collector.BatchSize = 100 },
			func(c *Config) { c.Collector.BatchSize = 200 },
			func(c *Config) bool { return c.Collector.BatchSize == 200 }, false},
		{"maintenance window is not a ceiling here", "trust.maintenance_window",
			func(c *Config) { c.Trust.MaintenanceWindow = "never" },
			func(c *Config) { c.Trust.MaintenanceWindow = "* * * * *" },
			func(c *Config) bool { return c.Trust.MaintenanceWindow == "* * * * *" },
			false},
	}
}

func TestRestrictTrustTakesTheMostRestrictive(t *testing.T) {
	for _, tc := range restrictTrustCases() {
		t.Run(tc.name, func(t *testing.T) {
			file, effective := DefaultConfig(), DefaultConfig()
			tc.file(file)
			tc.file(effective)
			tc.override(effective)
			if got := IsRestrictiveTrustKey(tc.key); got != tc.wantRestrictive {
				t.Fatalf("IsRestrictiveTrustKey(%q) = %v, want %v", tc.key, got,
					tc.wantRestrictive)
			}
			RestrictTrust(effective, file, tc.key)
			if !tc.check(effective) {
				t.Fatalf("after RestrictTrust %s: trust %+v collector %d", tc.key,
					effective.Trust, effective.Collector.BatchSize)
			}
		})
	}
	RestrictTrust(nil, DefaultConfig(), "trust.level") // must not panic
	RestrictTrust(DefaultConfig(), nil, "trust.level")
}
