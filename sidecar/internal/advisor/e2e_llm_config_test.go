//go:build e2e

package advisor

import "testing"

func TestE2ELLMConfigRequiresExplicitOptInAndPreservesGemini(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		auth   string
		absent bool
		bad    bool
	}{
		{name: "unconfigured", absent: true},
		{name: "native without live flag", env: map[string]string{"SAGE_LLM_AUTH": "databricks"}, absent: true},
		{name: "native missing route", env: map[string]string{"PG_SAGE_LIVE_LLM": "1", "SAGE_LLM_AUTH": "databricks"}, bad: true},
		{name: "native", auth: "databricks", env: map[string]string{"PG_SAGE_LIVE_LLM": "1", "SAGE_LLM_AUTH": "databricks", "SAGE_LLM_ENDPOINT": "https://workspace/ai-gateway/mlflow/v1", "SAGE_LLM_MODEL": "system.ai.test"}},
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
