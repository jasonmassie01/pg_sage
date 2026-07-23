package config

import "testing"

func TestAgentNativeDefaultsMatchPrescribedSafetyValues(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Policy.Profile != "unattended" {
		t.Fatalf("policy profile = %q, want unattended", cfg.Policy.Profile)
	}
	if cfg.Value.ToilModelVersion != 1 {
		t.Fatalf("toil model version = %d, want 1", cfg.Value.ToilModelVersion)
	}
	if cfg.Verify.WindowMinutes != 120 || cfg.Verify.WindowMaxMinutes != 4320 ||
		cfg.Verify.MinGainPct != 20 || cfg.Verify.RegressPct != 15 ||
		cfg.Verify.WriteImpactPct != 20 || cfg.Verify.MinSamples != 30 {
		t.Fatalf("verify defaults = %#v", cfg.Verify)
	}
	if cfg.Clone.Provider != "none" || cfg.Clone.MaxCloneAgeMinutes != 1440 {
		t.Fatalf("clone defaults = %#v", cfg.Clone)
	}
	if cfg.Custodian.Freeze.RedBufferPct != 25 {
		t.Fatalf("freeze defaults = %#v", cfg.Custodian.Freeze)
	}
	if cfg.Custodian.WAL.AbandonAfterMinutes != 1440 ||
		cfg.Custodian.WAL.RetainedWALDiskPctCeiling != 10 {
		t.Fatalf("WAL defaults = %#v", cfg.Custodian.WAL)
	}
	if !cfg.MCP.Enabled || cfg.MCP.Transport != "stdio" {
		t.Fatalf("MCP defaults = %#v", cfg.MCP)
	}
}

func TestAgentNativeYAMLLoadsEveryTopLevelGroup(t *testing.T) {
	cfg := DefaultConfig()
	yaml := `
policy:
  profile: staffed
value:
  toil_model_version: 2
verify:
  window_minutes: 30
  window_max_minutes: 60
  min_gain_pct: 25
  regress_pct: 10
  write_impact_pct: 12
  min_samples: 40
clone:
  provider: dle
  dle_endpoint: https://dle.example.invalid
  dle_token: ${TEST_DLE_TOKEN}
  max_clone_age_minutes: 120
custodian:
  freeze:
    red_buffer_pct: 20
  wal:
    abandon_after_minutes: 2880
    retained_wal_disk_pct_ceiling: 8
mcp:
  enabled: false
  transport: http
`
	t.Setenv("TEST_DLE_TOKEN", "secret-token")

	if err := loadYAML(writeYAMLFile(t, yaml), cfg); err != nil {
		t.Fatalf("loadYAML: %v", err)
	}
	if cfg.Policy.Profile != "staffed" || cfg.Value.ToilModelVersion != 2 {
		t.Fatalf("policy/value = %#v %#v", cfg.Policy, cfg.Value)
	}
	if cfg.Verify.WindowMinutes != 30 || cfg.Verify.MinSamples != 40 {
		t.Fatalf("verify = %#v", cfg.Verify)
	}
	if cfg.Clone.Provider != "dle" || cfg.Clone.DLEToken != "secret-token" {
		t.Fatalf("clone = %#v", cfg.Clone)
	}
	if cfg.Custodian.WAL.AbandonAfterMinutes != 2880 || cfg.MCP.Enabled {
		t.Fatalf("custodian/mcp = %#v %#v", cfg.Custodian, cfg.MCP)
	}
}

func TestAgentNativeConfigValidationFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"policy profile", func(cfg *Config) { cfg.Policy.Profile = "magic" }},
		{"verify window", func(cfg *Config) { cfg.Verify.WindowMinutes = 0 }},
		{
			"verify hard max",
			func(cfg *Config) { cfg.Verify.WindowMaxMinutes = cfg.Verify.WindowMinutes - 1 },
		},
		{"verify samples", func(cfg *Config) { cfg.Verify.MinSamples = 0 }},
		{"clone provider", func(cfg *Config) { cfg.Clone.Provider = "magic" }},
		{
			"dle endpoint",
			func(cfg *Config) {
				cfg.Clone.Provider = "dle"
				cfg.Clone.DLEEndpoint = ""
			},
		},
		{"mcp transport", func(cfg *Config) { cfg.MCP.Transport = "websocket" }},
		{
			"freeze percentage",
			func(cfg *Config) { cfg.Custodian.Freeze.RedBufferPct = 101 },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.edit(cfg)
			if err := cfg.validateAgentNative(); err == nil {
				t.Fatalf("validateAgentNative accepted %#v", cfg)
			}
		})
	}
}

func TestCloneDoesNotAliasAgentNativeConfig(t *testing.T) {
	cfg := DefaultConfig()
	clone := Clone(cfg)
	clone.Policy.Profile = "staffed"
	clone.Verify.MinSamples = 99

	if cfg.Policy.Profile == clone.Policy.Profile ||
		cfg.Verify.MinSamples == clone.Verify.MinSamples {
		t.Fatalf("Clone aliased agent-native config: original=%#v clone=%#v", cfg, clone)
	}
}

func TestAgentNativeEnvironmentOverrides(t *testing.T) {
	t.Setenv("SAGE_POLICY_PROFILE", "staffed")
	t.Setenv("SAGE_VERIFY_MIN_SAMPLES", "55")
	t.Setenv("SAGE_CLONE_PROVIDER", "snapshot")
	t.Setenv("SAGE_CUSTODIAN_WAL_ABANDON_AFTER_MINUTES", "90")
	t.Setenv("SAGE_MCP_ENABLED", "false")
	t.Setenv("SAGE_MCP_TRANSPORT", "http")
	cfg := DefaultConfig()
	overlayEnv(cfg)
	if cfg.Policy.Profile != "staffed" || cfg.Verify.MinSamples != 55 ||
		cfg.Clone.Provider != "snapshot" ||
		cfg.Custodian.WAL.AbandonAfterMinutes != 90 || cfg.MCP.Enabled ||
		cfg.MCP.Transport != "http" {
		t.Fatalf("agent-native environment overlay = %#v", cfg)
	}
}
