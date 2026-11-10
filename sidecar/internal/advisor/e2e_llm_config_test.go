package advisor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// e2eLLMConfig picks the advisor E2E model route from the environment. It is
// untagged so its contract test runs in every suite (ci-02); the billed E2Es
// that use it stay behind the e2e build tag.
func e2eLLMConfig(getenv func(string) string) (*config.LLMConfig, error) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		TimeoutSeconds:   60,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	if getenv("PG_SAGE_LIVE_LLM") == "1" && getenv("SAGE_LLM_AUTH") == "databricks" {
		cfg.Auth = "databricks"
		cfg.Endpoint, cfg.Model = getenv("SAGE_LLM_ENDPOINT"), getenv("SAGE_LLM_MODEL")
		if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Model) == "" {
			return nil, fmt.Errorf("native advisor E2E requires explicit " +
				"SAGE_LLM_ENDPOINT and SAGE_LLM_MODEL")
		}
		return cfg, nil
	}
	if cfg.APIKey = getenv("SAGE_GEMINI_API_KEY"); cfg.APIKey == "" {
		return nil, nil
	}
	cfg.Model = "gemini-2.5-flash"
	cfg.Endpoint = "https://generativelanguage.googleapis.com/v1beta/openai"
	return cfg, nil
}

func TestE2ELLMConfigRequiresExplicitOptInAndPreservesGemini(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		auth   string
		absent bool
		bad    bool
	}{
		{name: "unconfigured", absent: true},
		{name: "native without live flag",
			env: map[string]string{"SAGE_LLM_AUTH": "databricks"}, absent: true},
		{name: "native missing route",
			env: map[string]string{"PG_SAGE_LIVE_LLM": "1", "SAGE_LLM_AUTH": "databricks"},
			bad: true},
		{name: "native", auth: "databricks", env: map[string]string{
			"PG_SAGE_LIVE_LLM": "1", "SAGE_LLM_AUTH": "databricks",
			"SAGE_LLM_ENDPOINT": "https://workspace/ai-gateway/mlflow/v1",
			"SAGE_LLM_MODEL":    "system.ai.test"}},
		{name: "legacy Gemini", env: map[string]string{"SAGE_GEMINI_API_KEY": "synthetic-key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := e2eLLMConfig(func(key string) string { return tc.env[key] })
			if tc.bad {
				if err == nil {
					t.Fatal("incomplete native config accepted")
				}
				return
			}
			if err != nil || (cfg == nil) != tc.absent {
				t.Fatalf("config presence mismatch: cfg=%v error=%v", cfg != nil, err)
			}
			if cfg != nil && cfg.Auth != tc.auth {
				t.Fatalf("auth=%q, want %q", cfg.Auth, tc.auth)
			}
			if cfg != nil && tc.auth == "databricks" && cfg.APIKey != "" {
				t.Fatal("native fixture retained a static key")
			}
		})
	}
}
