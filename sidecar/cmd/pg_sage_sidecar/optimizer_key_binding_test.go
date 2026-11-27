package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestOptimizerLLMConfigBindsKeyToOrigin(t *testing.T) {
	parent := config.LLMConfig{
		Enabled: true, Endpoint: "https://general.example/v1", APIKey: "sk-general",
	}
	parent.OptimizerLLM = config.OptimizerLLMConfig{
		Enabled: true, Endpoint: "https://optimizer.example/v1",
	}
	if got := optimizerLLMConfig(parent); got.APIKey != "" ||
		got.Endpoint != "https://optimizer.example/v1" {
		t.Fatalf("other origin: endpoint %q key set=%v", got.Endpoint, got.APIKey != "")
	}
	parent.OptimizerLLM.Endpoint = "https://general.example/v2"
	if got := optimizerLLMConfig(parent); got.APIKey != "sk-general" {
		t.Fatal("same origin did not inherit the general key")
	}
	parent.OptimizerLLM.Endpoint = ""
	if got := optimizerLLMConfig(parent); got.APIKey != "sk-general" ||
		got.Endpoint != "https://general.example/v1" {
		t.Fatal("empty optimizer endpoint did not inherit endpoint and key")
	}
}
