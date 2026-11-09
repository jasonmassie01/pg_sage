package api

import (
	"strings"
	"testing"
)

// The databricks exemption must not launder a stored llm.api_key: an
// endpoint moved under llm.auth=databricks, followed by a switch back to
// api_key, would otherwise send the stored key to the moved endpoint.
func TestSecretBindingAuthLeavingDatabricksNeedsKey(t *testing.T) {
	cfg := bindingConfig()
	move := map[string]any{
		"llm.auth": "databricks", "llm.endpoint": "https://evil.example/serving",
	}
	if err := bindSecretDestinations(move, cfg); err != nil {
		t.Fatalf("move under databricks = %v, want nil (exempt)", err)
	}
	cfg.LLM.Auth, cfg.LLM.Endpoint = "databricks", "https://evil.example/serving"
	for _, body := range []map[string]any{
		{"llm.auth": "api_key"},
		{"llm.auth": ""},
		{"llm.auth": "api_key", "llm.api_key": "****cret"},
		{"llm.auth": "api_key", "llm.endpoint": "https://evil.example/v2"},
	} {
		err := bindSecretDestinations(body, cfg)
		if err == nil {
			t.Errorf("bind(%v) = nil, want refusal: stored key would reach the moved endpoint",
				body)
			continue
		}
		if !strings.Contains(err.Error(), "include llm.api_key, or send it empty") {
			t.Errorf("bind(%v) error %q lacks guidance", body, err)
		}
	}
	err := bindSecretDestinations(map[string]any{"llm.auth": "api_key"}, cfg)
	want := "llm.auth leaves databricks: include llm.api_key, or send it empty to clear it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestSecretBindingAuthLeavingDatabricksWithExplicitKey(t *testing.T) {
	cfg := bindingConfig()
	cfg.LLM.Auth = "databricks"
	for _, body := range []map[string]any{
		{"llm.auth": "api_key", "llm.api_key": "sk-new"},
		{"llm.auth": "api_key", "llm.api_key": ""},
		{"llm.auth": "databricks"},      // stays databricks
		{"llm.model": "served-model"},   // auth unchanged
		{"llm.auth": "api_key", "x": 1}, // checked below with no stored key
	} {
		current := cfg
		if _, ok := body["x"]; ok {
			clone := *cfg
			clone.LLM.APIKey = ""
			current = &clone
		}
		if err := bindSecretDestinations(body, current); err != nil {
			t.Errorf("bind(%v) = %v, want nil", body, err)
		}
	}
	// Moving the endpoint while leaving databricks names the endpoint.
	err := bindSecretDestinations(map[string]any{
		"llm.auth": "api_key", "llm.endpoint": "https://other.example/v1",
	}, cfg)
	if err == nil || !strings.Contains(err.Error(), "llm.endpoint moves to a new origin") {
		t.Fatalf("error = %v, want the endpoint move named", err)
	}
}
