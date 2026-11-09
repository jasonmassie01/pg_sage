package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

func storedOverride(t *testing.T, cs *store.ConfigStore, key string) (string, bool) {
	t.Helper()
	overrides, err := cs.GetOverrides(t.Context(), 0)
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}
	for _, o := range overrides {
		if o.Key == key {
			return o.Value, true
		}
	}
	return "", false
}

// An empty secret sent with a new origin clears the stored secret in the
// same request; the new endpoint is then saved without any credential.
func TestSecretBindingEmptyKeyClearsOnNewOrigin(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := bindingConfig()
	handler := configGlobalPutHandler(cs, cfg, nil)
	w := doRequestWithUserRetry(t, handler, "PUT", "/api/v1/config/global",
		`{"llm.endpoint":"https://other.example/v1","llm.api_key":""}`, user)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", w.Code, w.Body.String())
	}
	if cfg.LLM.APIKey != "" || cfg.LLM.Endpoint != "https://other.example/v1" {
		t.Fatalf("hot config = endpoint %q key set=%v, want new endpoint and no key",
			cfg.LLM.Endpoint, cfg.LLM.APIKey != "")
	}
	if v, ok := storedOverride(t, cs, "llm.api_key"); !ok || v != "" {
		t.Fatalf("stored llm.api_key = %q (present=%v), want empty override", v, ok)
	}
}

// Refused writes persist nothing.
func TestSecretBindingRefusedWritePersistsNothing(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := bindingConfig()
	handler := configGlobalPutHandler(cs, cfg, nil)
	w := doRequestWithUserRetry(t, handler, "PUT", "/api/v1/config/global",
		`{"llm.endpoint":"https://other.example/v1","llm.model":"m2"}`, user)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d %s, want 400", w.Code, w.Body.String())
	}
	for _, key := range []string{"llm.endpoint", "llm.model", "llm.api_key"} {
		if v, ok := storedOverride(t, cs, key); ok {
			t.Errorf("refused write persisted %s=%q", key, v)
		}
	}
	if cfg.LLM.Model == "m2" {
		t.Fatal("refused write hot-reloaded llm.model")
	}
}

// Same-origin edits with a masked secret keep working (must not change).
func TestSecretBindingSameOriginMaskedKeySaves(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := bindingConfig()
	handler := configGlobalPutHandler(cs, cfg, nil)
	w := doRequestWithUserRetry(t, handler, "PUT", "/api/v1/config/global",
		`{"llm.endpoint":"https://llm.example/v2","llm.api_key":"****cret"}`, user)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", w.Code, w.Body.String())
	}
	if cfg.LLM.Endpoint != "https://llm.example/v2" || cfg.LLM.APIKey != "sk-stored-secret" {
		t.Fatalf("config = %q / key kept=%v",
			cfg.LLM.Endpoint, cfg.LLM.APIKey == "sk-stored-secret")
	}
	if _, ok := storedOverride(t, cs, "llm.api_key"); ok {
		t.Fatal("masked key was persisted as an override")
	}
}
