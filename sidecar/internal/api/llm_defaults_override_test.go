package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// A persisted override (Settings UI / API, stored in sage.config) of
// "false" must win over the new default-on LLM switches at startup.

type llmOverrideSwitch struct {
	key string
	get func(*config.Config) bool
}

var llmOverrideSwitches = []llmOverrideSwitch{
	{"llm.enabled", func(c *config.Config) bool { return c.LLM.Enabled }},
	{"llm.optimizer.enabled", func(c *config.Config) bool {
		return c.LLM.Optimizer.Enabled
	}},
	{"advisor.enabled", func(c *config.Config) bool { return c.Advisor.Enabled }},
	{"tuner.llm_enabled", func(c *config.Config) bool { return c.Tuner.LLMEnabled }},
	{"rca.narration_enabled", func(c *config.Config) bool {
		return c.RCA.NarrationEnabled
	}},
	{"explain.enabled", func(c *config.Config) bool { return c.Explain.Enabled }},
}

func TestPersistedFalseOverrideBeatsLLMDefaults(t *testing.T) {
	for _, sw := range llmOverrideSwitches {
		t.Run(sw.key, func(t *testing.T) {
			candidate := config.DefaultConfig()
			if !sw.get(candidate) {
				t.Fatalf("precondition: %s defaults to true", sw.key)
			}
			ApplyConfigOverrideSnapshot(candidate, sw.key, "false")
			if sw.get(candidate) {
				t.Fatalf("%s: persisted false override ignored", sw.key)
			}
			for _, other := range llmOverrideSwitches {
				if other.key != sw.key && !other.get(candidate) {
					t.Errorf("%s=false also turned off %s", sw.key, other.key)
				}
			}
		})
	}
}

func TestPersistedTrueOverrideRestoresLLMSwitch(t *testing.T) {
	for _, sw := range llmOverrideSwitches {
		candidate := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(candidate, sw.key, "false")
		ApplyConfigOverrideSnapshot(candidate, sw.key, "true")
		if !sw.get(candidate) {
			t.Errorf("%s: true after false did not re-enable", sw.key)
		}
	}
}

// Anything but the literal "true" fails closed to off.
func TestPersistedNonBooleanOverrideTurnsLLMSwitchOff(t *testing.T) {
	for _, value := range []string{"", "yes", "1", "TRUE "} {
		candidate := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(candidate, "llm.enabled", value)
		if candidate.LLM.Enabled {
			t.Errorf("llm.enabled override %q left the LLM on", value)
		}
	}
}

// The persisted budget override is applied verbatim, including the
// 0 = no cap boundary.
func TestPersistedTokenBudgetOverrideBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"0", 0}, {"1", 1}, {"750000", 750000}} {
		candidate := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(candidate, "llm.token_budget_daily", tc.value)
		if candidate.LLM.TokenBudgetDaily != tc.want {
			t.Errorf("override %q: budget = %d, want %d", tc.value,
				candidate.LLM.TokenBudgetDaily, tc.want)
		}
	}
}
