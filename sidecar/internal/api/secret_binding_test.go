package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/llm"
)

func bindingConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.LLM.Endpoint = "https://llm.example/v1"
	cfg.LLM.APIKey = "sk-stored-secret"
	cfg.Clone.DLEEndpoint = "https://dle.example:2345"
	cfg.Clone.DLEToken = "dle-stored-token"
	return cfg
}

func TestSecretBindingRefusesNewOriginWithoutSecret(t *testing.T) {
	cases := []map[string]any{
		{"llm.endpoint": "https://other.example/v1"},
		{"clone.dle_endpoint": "https://other.example:2345"},
		{"llm.endpoint": "http://llm.example/v1"},    // scheme change
		{"llm.endpoint": "https://llm.example:8443"}, // port change
		{"llm.endpoint": "https://other.example/v1", "llm.api_key": "****cret"},
		{"llm.endpoint": "https://other.example/v1", "llm.api_key": "********"},
		{"clone.dle_endpoint": "https://other.example", "clone.dle_token": "****"},
	}
	for _, body := range cases {
		err := bindSecretDestinations(body, bindingConfig())
		if err == nil {
			t.Errorf("bind(%v) = nil, want refusal", body)
			continue
		}
		if !strings.Contains(err.Error(), "moves to a new origin") {
			t.Errorf("bind(%v) error %q lacks guidance", body, err)
		}
	}
}

func TestSecretBindingErrorNamesTheSecret(t *testing.T) {
	err := bindSecretDestinations(
		map[string]any{"clone.dle_endpoint": "https://x.example"}, bindingConfig())
	want := "clone.dle_endpoint moves to a new origin: include clone.dle_token, " +
		"or send it empty to clear it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	err = bindSecretDestinations(
		map[string]any{"llm.endpoint": "https://x.example"}, bindingConfig())
	want = "llm.endpoint moves to a new origin: include llm.api_key, " +
		"or send it empty to clear it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestSecretBindingAllowsSameOriginAndExplicitSecrets(t *testing.T) {
	cases := []map[string]any{
		{"llm.endpoint": "https://llm.example/v2/chat"},     // path only
		{"llm.endpoint": "https://llm.example:443/v1"},      // effective port
		{"llm.endpoint": "https://LLM.example/v1"},          // host case
		{"llm.endpoint": "https://llm.example/v1", "x": 1},  // unchanged
		{"clone.dle_endpoint": "https://dle.example:2345/"}, // trailing slash
		{"llm.endpoint": "https://other.example/v1", "llm.api_key": "sk-new"},
		{"llm.endpoint": "https://other.example/v1", "llm.api_key": ""},
		{"clone.dle_endpoint": "https://other.example", "clone.dle_token": ""},
		{"clone.dle_endpoint": "https://other.example", "clone.dle_token": "tok"},
		{"llm.api_key": "****cret"}, // masked secret with no endpoint change
		{"llm.model": "gpt-x"},
		{},
	}
	for _, body := range cases {
		if err := bindSecretDestinations(body, bindingConfig()); err != nil {
			t.Errorf("bind(%v) = %v, want nil", body, err)
		}
	}
}

func TestSecretBindingNilAndEmptyState(t *testing.T) {
	if err := bindSecretDestinations(nil, bindingConfig()); err != nil {
		t.Fatalf("nil body = %v, want nil", err)
	}
	body := map[string]any{"llm.endpoint": "https://other.example"}
	if err := bindSecretDestinations(body, nil); err != nil {
		t.Fatalf("nil current config = %v, want nil (nothing stored to leak)", err)
	}
	empty := config.DefaultConfig()
	empty.LLM.APIKey, empty.LLM.Endpoint = "", "https://llm.example"
	if err := bindSecretDestinations(body, empty); err != nil {
		t.Fatalf("no stored key = %v, want nil", err)
	}
	unset := bindingConfig()
	unset.LLM.Endpoint = ""
	if err := bindSecretDestinations(body, unset); err == nil {
		t.Fatal("setting an endpoint where none was, with a stored key, was allowed")
	}
}

func TestSecretBindingSkipsDatabricksAuth(t *testing.T) {
	cfg := bindingConfig()
	cfg.LLM.Auth = "databricks"
	body := map[string]any{"llm.endpoint": "https://other.example/serving"}
	if err := bindSecretDestinations(body, cfg); err != nil {
		t.Fatalf("databricks auth = %v, want nil", err)
	}
	cfg.LLM.Auth = "api_key"
	body["llm.auth"] = "databricks"
	if err := bindSecretDestinations(body, cfg); err != nil {
		t.Fatalf("switching to databricks auth = %v, want nil", err)
	}
	cfg.LLM.Auth = "databricks"
	body["llm.auth"] = "api_key"
	if err := bindSecretDestinations(body, cfg); err == nil {
		t.Fatal("leaving databricks auth for api_key with a new origin was allowed")
	}
	// The DLE pair is not covered by the databricks exemption.
	dle := map[string]any{"clone.dle_endpoint": "https://other.example"}
	if err := bindSecretDestinations(dle, cfg); err == nil {
		t.Fatal("databricks exemption leaked to clone.dle_endpoint")
	}
}

func TestSecretBindingPutRefusedLeavesConfig(t *testing.T) {
	cfg := bindingConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	handler := configGlobalPutHandler(nil, controller)
	for _, body := range []string{
		`{"expected_generation":1,"llm.endpoint":"https://other.example/v1"}`,
		`{"expected_generation":1,"clone.dle_endpoint":"https://other.example"}`,
		`{"expected_generation":1,"llm.endpoint":"https://other.example/v1",` +
			`"llm.api_key":"****cret"}`,
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(
			http.MethodPut, "/api/v1/config/global", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest ||
			!strings.Contains(w.Body.String(), "moves to a new origin") {
			t.Fatalf("PUT %s = %d %s, want 400 new origin", body, w.Code, w.Body.String())
		}
	}
	desired := controller.Desired()
	if desired.Generation != 1 {
		t.Fatalf("desired generation = %d after refusals, want 1", desired.Generation)
	}
	got := desired.Config
	if got.LLM.APIKey != "sk-stored-secret" || got.LLM.Endpoint != "https://llm.example/v1" {
		t.Fatalf("llm config changed after refusal: %+v", got.LLM)
	}
	if got.Clone.DLEToken != "dle-stored-token" ||
		got.Clone.DLEEndpoint != "https://dle.example:2345" {
		t.Fatalf("clone config changed after refusal: %+v", got.Clone)
	}
}

func TestSecretBindingControlledPutRefused(t *testing.T) {
	cfg := bindingConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	handler := configGlobalPutHandler(nil, controller)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/global",
		strings.NewReader(`{"expected_generation":1,"llm.endpoint":"https://evil.example"}`)))
	if w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "include llm.api_key") {
		t.Fatalf("controlled PUT = %d %s, want 400", w.Code, w.Body.String())
	}
	desired := controller.Desired()
	if desired.Generation != 1 || desired.Config.LLM.Endpoint != "https://llm.example/v1" {
		t.Fatalf("desired changed: gen %d endpoint %q",
			desired.Generation, desired.Config.LLM.Endpoint)
	}
}

// CHECK-03: after an endpoint-only PUT is refused, the key keeps going to
// the old origin and the new origin never sees an Authorization header.
func TestSecretBindingNewOriginReceivesNoAuthorization(t *testing.T) {
	var newHits, oldAuth atomic.Int64
	newOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer newOrigin.Close()
	oldOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-stored-secret" {
			oldAuth.Add(1)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer oldOrigin.Close()
	cfg := bindingConfig()
	cfg.LLM.Endpoint = oldOrigin.URL + "/v1"
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	handler := configGlobalPutHandler(nil, controller)
	w := httptest.NewRecorder()
	body := `{"expected_generation":1,"llm.endpoint":"` + newOrigin.URL + `/v1"}`
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/global",
		strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("endpoint-only PUT = %d, want 400", w.Code)
	}
	desired := controller.Desired().Config
	if _, err := llm.ListModelsWithClient(t.Context(), desired.LLM.Endpoint,
		desired.LLM.APIKey, egress.HTTPClient(0)); err != nil {
		t.Fatalf("list models at the configured origin: %v", err)
	}
	if newHits.Load() != 0 || oldAuth.Load() != 1 {
		t.Fatalf("new origin hits %d, old origin authorized %d; want 0 and 1",
			newHits.Load(), oldAuth.Load())
	}
}

// Concurrent reads of the same config: bindSecretDestinations must not
// mutate the body or the config it is given.
func TestSecretBindingConcurrentCallsDoNotMutate(t *testing.T) {
	cfg := bindingConfig()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := map[string]any{"llm.endpoint": "https://other.example"}
			_ = bindSecretDestinations(body, cfg)
			if len(body) != 1 {
				t.Errorf("body mutated: %v", body)
			}
		}()
	}
	wg.Wait()
	if cfg.LLM.APIKey != "sk-stored-secret" {
		t.Fatalf("config mutated: %q", cfg.LLM.APIKey)
	}
}
