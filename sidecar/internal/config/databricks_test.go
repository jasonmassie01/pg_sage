package config

import "testing"

func TestDatabricksLLMAuthenticationConfig(t *testing.T) {
	t.Setenv("SAGE_LLM_AUTH", "databricks")
	t.Setenv("SAGE_LLM_ENDPOINT", "https://example.cloud.databricks.com/ai-gateway/mlflow/v1")
	t.Setenv("SAGE_LLM_API_KEY", "")
	cfg, err := Load([]string{"--pg-url", "postgres://test@localhost/test"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Auth != "databricks" || cfg.LLMSetupNotice() != "" {
		t.Fatal("unified authentication should enable configured LLM without a static key")
	}
	cfg.LLM.Auth = "typo"
	if err := cfg.LLM.validateWire(); err == nil {
		t.Fatal("invalid authentication mode accepted")
	}
}

func TestOptimizerDoesNotSendDatabricksCredentialsToOtherProviders(t *testing.T) {
	parent := LLMConfig{Auth: "databricks", Endpoint: "https://workspace/ai-gateway/mlflow/v1"}
	if parent.OptimizerAuth() != "databricks" {
		t.Fatal("same-provider optimizer lost unified authentication")
	}
	parent.OptimizerLLM.Endpoint = "https://other-provider/v1"
	if parent.OptimizerAuth() != "api_key" {
		t.Fatal("optimizer inherited workspace authentication for another provider")
	}
}
