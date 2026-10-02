package config

import (
	"strings"
	"testing"
)

// LLM features are on by default: pg_sage is an AI DBA. Effective use
// still needs llm.endpoint and llm.api_key; without them every feature
// runs its deterministic path.

// loadNoConfigFile loads with no YAML file and no LLM environment, the
// "default value masking" setup: only the built-in defaults apply.
func loadNoConfigFile(t *testing.T) *Config {
	t.Helper()
	chdirTemp(t)
	for _, key := range []string{
		"SAGE_LLM_API_KEY", "SAGE_LLM_ENDPOINT", "SAGE_LLM_MODEL",
		"SAGE_OPTIMIZER_LLM_API_KEY", "SAGE_OPTIMIZER_LLM_ENDPOINT",
		"SAGE_OPTIMIZER_LLM_MODEL", "GEMINI_API_KEY",
	} {
		t.Setenv(key, "")
	}
	cfg, err := Load([]string{"--pg-url", "postgres://u@localhost/db"})
	if err != nil {
		t.Fatalf("Load without a config file: %v", err)
	}
	if cfg.ConfigPath != "" {
		t.Fatalf("ConfigPath = %q, want no config file", cfg.ConfigPath)
	}
	return cfg
}

type llmSwitch struct {
	key  string
	get  func(*Config) bool
	yaml string
}

// llmSwitches are the LLM-backed feature switches that default to on.
var llmSwitches = []llmSwitch{
	{"llm.enabled", func(c *Config) bool { return c.LLM.Enabled },
		"llm:\n  enabled: false\n"},
	{"llm.optimizer.enabled", func(c *Config) bool { return c.LLM.Optimizer.Enabled },
		"llm:\n  optimizer:\n    enabled: false\n"},
	{"advisor.enabled", func(c *Config) bool { return c.Advisor.Enabled },
		"advisor:\n  enabled: false\n"},
	{"tuner.llm_enabled", func(c *Config) bool { return c.Tuner.LLMEnabled },
		"tuner:\n  llm_enabled: false\n"},
	{"rca.narration_enabled", func(c *Config) bool { return c.RCA.NarrationEnabled },
		"rca:\n  narration_enabled: false\n"},
	{"explain.enabled", func(c *Config) bool { return c.Explain.Enabled },
		"explain:\n  enabled: false\n"},
}

func TestLLMFeaturesDefaultOnWithoutConfigFile(t *testing.T) {
	cfg := loadNoConfigFile(t)
	for _, sw := range llmSwitches {
		if !sw.get(cfg) {
			t.Errorf("%s = false with no config file, want true", sw.key)
		}
	}
	if !cfg.LLM.IndexOptimizer.Enabled {
		t.Error("deprecated llm.index_optimizer.enabled = false, want true " +
			"(mirrors llm.optimizer.enabled)")
	}
	if cfg.LLM.Endpoint != "" || cfg.LLM.APIKey != "" {
		t.Fatalf("defaults carry endpoint %q / key %q, want none",
			cfg.LLM.Endpoint, cfg.LLM.APIKey)
	}
}

func TestDefaultConfigMatchesLoadedLLMDefaults(t *testing.T) {
	loaded := loadNoConfigFile(t)
	built := DefaultConfig()
	for _, sw := range llmSwitches {
		if sw.get(built) != sw.get(loaded) {
			t.Errorf("%s: DefaultConfig=%v, Load=%v", sw.key,
				sw.get(built), sw.get(loaded))
		}
	}
	if built.LLM.TokenBudgetDaily != loaded.LLM.TokenBudgetDaily {
		t.Errorf("token_budget_daily: DefaultConfig=%d, Load=%d",
			built.LLM.TokenBudgetDaily, loaded.LLM.TokenBudgetDaily)
	}
}

// LLM defaults grant no autonomy: trust, tiers, execution switches and
// the dedicated optimizer tier keep their previous defaults.
func TestLLMDefaultsLeaveExecutionDefaultsUnchanged(t *testing.T) {
	cfg := loadNoConfigFile(t)
	if cfg.Trust.Level != "observation" {
		t.Errorf("trust.level = %q, want observation", cfg.Trust.Level)
	}
	if cfg.Trust.Tier3Moderate || cfg.Trust.Tier3HighRisk {
		t.Errorf("tier3_moderate=%v tier3_high_risk=%v, want false/false",
			cfg.Trust.Tier3Moderate, cfg.Trust.Tier3HighRisk)
	}
	if !cfg.Trust.Tier3Safe {
		t.Error("tier3_safe = false, want its existing default true")
	}
	if cfg.Runaway.Enabled {
		t.Error("runaway.enabled = true, want false (it cancels queries)")
	}
	if !cfg.Tuner.Enabled {
		t.Error("tuner.enabled = false, want its existing default true")
	}
	if cfg.LLM.OptimizerLLM.Enabled {
		t.Error("llm.optimizer_llm.enabled = true, want false: a second " +
			"client with its own budget is opt-in")
	}
	if !cfg.LLM.OptimizerLLM.FallbackToGeneral {
		t.Error("optimizer_llm.fallback_to_general = false, want true")
	}
}

func TestLLMTokenBudgetDefaultIsNonZero(t *testing.T) {
	if DefaultLLMTokenBudget <= 0 {
		t.Fatalf("DefaultLLMTokenBudget = %d, want > 0", DefaultLLMTokenBudget)
	}
	cfg := loadNoConfigFile(t)
	if !cfg.LLM.Enabled {
		t.Fatal("precondition: llm.enabled defaults to true")
	}
	if cfg.LLM.TokenBudgetDaily != 500000 {
		t.Errorf("llm.token_budget_daily = %d, want 500000",
			cfg.LLM.TokenBudgetDaily)
	}
	if cfg.LLM.OptimizerLLM.TokenBudgetDaily != 500000 {
		t.Errorf("llm.optimizer_llm.token_budget_daily = %d, want 500000",
			cfg.LLM.OptimizerLLM.TokenBudgetDaily)
	}
	if cfg.LLM.FleetTokenBudgetDaily != 0 {
		t.Errorf("llm.fleet_token_budget_daily = %d, want 0 (off)",
			cfg.LLM.FleetTokenBudgetDaily)
	}
}

// A partial llm section must not zero the budget (default masking).
func TestLLMTokenBudgetSurvivesPartialLLMSection(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  model: gpt-4o-mini\n")
	if cfg.LLM.TokenBudgetDaily != DefaultLLMTokenBudget {
		t.Errorf("token_budget_daily = %d, want %d", cfg.LLM.TokenBudgetDaily,
			DefaultLLMTokenBudget)
	}
	if !cfg.LLM.Enabled || cfg.LLM.Model != "gpt-4o-mini" {
		t.Errorf("enabled=%v model=%q, want true/gpt-4o-mini",
			cfg.LLM.Enabled, cfg.LLM.Model)
	}
}

// Boundaries: 0 is the documented "no cap" value; 1 is the smallest cap.
func TestLLMTokenBudgetExplicitBoundariesHonoured(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{
		{"llm:\n  token_budget_daily: 0\n", 0},
		{"llm:\n  token_budget_daily: 1\n", 1},
		{"llm:\n  token_budget_daily: 2000000\n", 2000000},
	} {
		cfg := loadYAMLConfig(t, tc.yaml)
		if cfg.LLM.TokenBudgetDaily != tc.want {
			t.Errorf("%q: token_budget_daily = %d, want %d", tc.yaml,
				cfg.LLM.TokenBudgetDaily, tc.want)
		}
	}
}

func TestLLMTokenBudgetRejectsNonNumeric(t *testing.T) {
	chdirTemp(t)
	path := writeYAMLFile(t, "llm:\n  token_budget_daily: lots\n")
	if _, err := Load([]string{"--config", path,
		"--pg-url", "postgres://u@localhost/db"}); err == nil {
		t.Fatal("non-numeric token_budget_daily was accepted")
	}
}

// loadYAMLConfig loads body through Load with no LLM environment.
func loadYAMLConfig(t *testing.T, body string) *Config {
	t.Helper()
	chdirTemp(t)
	t.Setenv("SAGE_LLM_API_KEY", "")
	t.Setenv("SAGE_LLM_ENDPOINT", "")
	path := writeYAMLFile(t, body)
	cfg, err := Load([]string{"--config", path,
		"--pg-url", "postgres://u@localhost/db"})
	if err != nil {
		t.Fatalf("Load(%q): %v", body, err)
	}
	return cfg
}

func TestExplicitFalseInYAMLHonouredForEveryLLMSwitch(t *testing.T) {
	for _, sw := range llmSwitches {
		t.Run(sw.key, func(t *testing.T) {
			cfg := loadYAMLConfig(t, sw.yaml)
			if sw.get(cfg) {
				t.Fatalf("%s: explicit false in YAML loaded as true", sw.key)
			}
			for _, other := range llmSwitches {
				if other.key != sw.key && !other.get(cfg) {
					t.Errorf("%s: false leaked into %s", sw.key, other.key)
				}
			}
		})
	}
}

func TestExplicitTrueInYAMLKeepsLLMSwitchesOn(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  enabled: true\n  optimizer:\n"+
		"    enabled: true\nadvisor:\n  enabled: true\ntuner:\n"+
		"  llm_enabled: true\nrca:\n  narration_enabled: true\n")
	for _, sw := range llmSwitches {
		if !sw.get(cfg) {
			t.Errorf("%s = false after explicit true", sw.key)
		}
	}
}

func TestLLMSwitchRejectsNonBoolean(t *testing.T) {
	chdirTemp(t)
	path := writeYAMLFile(t, "llm:\n  enabled: sometimes\n")
	_, err := Load([]string{"--config", path, "--pg-url", "postgres://u@h/db"})
	// yaml.v3 names the line and the offending value, not the key.
	if err == nil || !strings.Contains(err.Error(), "line 2") ||
		!strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("Load err = %v, want a parse error naming line 2 and the value", err)
	}
}
