package config

import (
	"context"
	"strings"
	"testing"
)

func TestDatabricksAppAIRejectsDowngradesBeforePersistence(t *testing.T) {
	t.Setenv("SAGE_AUTH_MODE", "databricks_app")
	t.Setenv("DATABRICKS_HOST", "https://workspace.cloud.databricks.com")
	base := DefaultConfig()
	base.LLM.Auth = "databricks"
	base.LLM.Endpoint = "https://workspace.cloud.databricks.com/ai-gateway/mlflow/v1"
	controller := NewConfigController(base, nil)
	for _, tc := range []struct {
		name   string
		mutate func(*LLMConfig)
	}{
		{"auth", func(l *LLMConfig) { l.Auth = "api_key" }},
		{"general key", func(l *LLMConfig) { l.APIKey = "test-key" }},
		{"optimizer key", func(l *LLMConfig) { l.OptimizerLLM.APIKey = "test-key" }},
		{"other origin", func(l *LLMConfig) { l.Endpoint = "https://other.example/v1" }},
		{"other optimizer origin", func(l *LLMConfig) { l.OptimizerLLM.Endpoint = "https://other.example/v1" }},
		{"other port", func(l *LLMConfig) { l.Endpoint = "https://workspace.cloud.databricks.com:444/v1" }},
		{"query credentials", func(l *LLMConfig) { l.Endpoint += "?key=test-key" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := Clone(base)
			tc.mutate(&candidate.LLM)
			persisted := false
			_, err := controller.ApplyWithPersistence(context.Background(), 1, candidate, func(context.Context, ConfigSnapshot) error { persisted = true; return nil })
			if err == nil || persisted || controller.Active().Generation != 1 || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("unsafe candidate accepted/persisted/disclosed: err=%v persisted=%v", err, persisted)
			}
		})
	}
}

func TestDatabricksAppDedicatedOptimizerKeepsOAuth(t *testing.T) {
	t.Setenv("SAGE_AUTH_MODE", "databricks_app")
	t.Setenv("DATABRICKS_HOST", "https://workspace.cloud.databricks.com")
	l := LLMConfig{Auth: "databricks", Endpoint: "https://workspace.cloud.databricks.com/ai-gateway/mlflow/v1"}
	l.OptimizerLLM.Endpoint = "https://workspace.cloud.databricks.com/serving-endpoints"
	if err := l.ValidateDatabricksApp(); err != nil || l.OptimizerAuth() != "databricks" {
		t.Fatalf("dedicated workspace optimizer lost OAuth: %v", err)
	}
	t.Setenv("SAGE_AUTH_MODE", "")
	t.Setenv("DATABRICKS_APP_PORT", "")
	if l.OptimizerAuth() != "api_key" {
		t.Fatal("standalone provider behavior changed")
	}
}
