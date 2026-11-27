package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// fileConfig is the config file: key and endpoint saved together.
func fileConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.LLM.Endpoint, cfg.LLM.APIKey = "https://llm.example/v1", "sk-file"
	cfg.Clone.DLEEndpoint, cfg.Clone.DLEToken = "https://dle.example", "dle-file"
	return cfg
}

func withLLM(cfg *config.Config, auth, endpoint, key string) *config.Config {
	out := config.Clone(cfg)
	out.LLM.Auth, out.LLM.Endpoint, out.LLM.APIKey = auth, endpoint, key
	return out
}

// Deleting the llm.api_key override brings back the file's key; with an
// endpoint override on another origin it would go there.
func TestSecretBindingDeleteKeyRefusedOnMovedEndpoint(t *testing.T) {
	base := fileConfig()
	current := withLLM(base, "", "https://evil.example/v1", "junk")
	candidate := withLLM(base, "", "https://evil.example/v1", "sk-file")
	err := deleteBindingError("llm.api_key", current, candidate, base)
	want := "deleting llm.api_key would send the config file's llm.api_key to " +
		"llm.endpoint on another origin: delete llm.endpoint first, or set llm.api_key"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	dleCurrent := config.Clone(base)
	dleCurrent.Clone.DLEEndpoint, dleCurrent.Clone.DLEToken = "https://evil.example", "x"
	dleCandidate := config.Clone(dleCurrent)
	dleCandidate.Clone.DLEToken = "dle-file"
	if err := deleteBindingError("clone.dle_token", dleCurrent, dleCandidate, base); err == nil ||
		!strings.Contains(err.Error(), "delete clone.dle_endpoint first") {
		t.Fatalf("DLE token delete error = %v, want refusal", err)
	}
}

func TestSecretBindingDeleteAllowedCases(t *testing.T) {
	base := fileConfig()
	cases := []struct {
		name               string
		key                string
		current, candidate *config.Config
	}{
		{"key back on the file's origin, other path", "llm.api_key",
			withLLM(base, "", "https://llm.example/v2", "sk-api"),
			withLLM(base, "", "https://llm.example/v2", "sk-file")},
		{"endpoint back to the file's, API key kept", "llm.endpoint",
			withLLM(base, "", "https://other.example", "sk-api"),
			withLLM(base, "", "https://llm.example/v1", "sk-api")},
		{"endpoint back to the file's, file key", "llm.endpoint",
			withLLM(base, "", "https://other.example", "sk-file"),
			withLLM(base, "", "https://llm.example/v1", "sk-file")},
		{"no secret after delete", "llm.api_key",
			withLLM(base, "", "https://evil.example", "x"),
			withLLM(base, "", "https://evil.example", "")},
		{"databricks after delete", "llm.api_key",
			withLLM(base, "databricks", "https://ws.example", "x"),
			withLLM(base, "databricks", "https://ws.example", "sk-file")},
		{"unrelated key", "llm.model",
			withLLM(base, "", "https://evil.example", "x"),
			withLLM(base, "", "https://evil.example", "sk-file")},
		{"auth delete with everything from the file", "llm.auth",
			withLLM(base, "databricks", "https://llm.example/v1", "sk-file"),
			withLLM(base, "", "https://llm.example/v1", "sk-file")},
	}
	for _, tc := range cases {
		if err := deleteBindingError(tc.key, tc.current, tc.candidate, base); err != nil {
			t.Errorf("%s: %v, want nil", tc.name, err)
		}
	}
}

// Leaving databricks auth by deleting the llm.auth override: the endpoint
// may have moved without the key while the exemption applied.
func TestSecretBindingDeleteAuthLeavingDatabricks(t *testing.T) {
	base := fileConfig()
	for name, tc := range map[string][2]*config.Config{
		"API key": {withLLM(base, "databricks", "https://evil.example", "sk-api"),
			withLLM(base, "", "https://evil.example", "sk-api")},
		"file key": {withLLM(base, "databricks", "https://evil.example", "sk-file"),
			withLLM(base, "api_key", "https://evil.example", "sk-file")},
	} {
		err := deleteBindingError("llm.auth", tc[0], tc[1], base)
		if err == nil || !strings.Contains(err.Error(),
			"deleting llm.auth would send the stored llm.api_key") {
			t.Errorf("%s: error = %v, want refusal", name, err)
		}
	}
}

func TestSecretBindingDeleteMayRebind(t *testing.T) {
	for key, want := range map[string]bool{
		"llm.endpoint": true, "llm.api_key": true, "llm.auth": true,
		"clone.dle_endpoint": true, "clone.dle_token": true,
		"llm.model": false, "trust.level": false, "": false,
	} {
		if got := deleteMayRebind(key); got != want {
			t.Errorf("deleteMayRebind(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestSecretBindingDeleteRefusalErrors(t *testing.T) {
	base := fileConfig()
	baseErr := errors.New("config file unreadable")
	failing := func() (*config.Config, error) { return nil, baseErr }
	_, err := boundDeleteRefusal(t.Context(), nil, failing, "llm.api_key", base, base)
	if !errors.Is(err, baseErr) || !strings.Contains(err.Error(), "load config base") {
		t.Fatalf("base failure = %v, want wrapped base error", err)
	}
	// Unrelated keys and a nil current config never load anything.
	for _, tc := range []struct {
		key     string
		current *config.Config
	}{{"llm.model", base}, {"llm.api_key", nil}} {
		msg, err := boundDeleteRefusal(t.Context(), nil, failing, tc.key, tc.current, base)
		if msg != "" || err != nil {
			t.Errorf("%s: (%q, %v), want no check", tc.key, msg, err)
		}
	}
	current := withLLM(base, "", "https://evil.example", "x")
	candidate := withLLM(base, "", "https://evil.example", "sk-file")
	msg, err := boundDeleteRefusal(
		t.Context(), nil, staticConfigBase(base), "llm.api_key", current, candidate)
	if err != nil || !strings.HasPrefix(msg, "deleting llm.api_key would send") {
		t.Fatalf("refusal = (%q, %v), want message", msg, err)
	}
}

// Through the DB: an API endpoint override plus a throwaway key, then a
// delete of the key, must not bring the file's key to the new origin.
func TestSecretBindingDeleteHandlerRefusesFileKeyOnNewOrigin(t *testing.T) {
	cs, user := contractSetup(t)
	base := bindingConfig()
	controller := config.NewConfigController(config.Clone(base), nil, apiTrustOwner{})
	put := configGlobalPutHandler(cs, controller)
	w := doRequestWithUserRetry(t, put, "PUT", "/api/v1/config/global",
		`{"expected_generation":1,"llm.endpoint":"https://evil.example/v1",`+
			`"llm.api_key":"junk"}`, user)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", w.Code, w.Body.String())
	}
	del := configGlobalDeleteHandler(cs, staticConfigBase(base), controller)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/config/global/{key}", del)
	deleteKey := func(key string) *httptest.ResponseRecorder {
		path := fmt.Sprintf("/api/v1/config/global/%s?expected_generation=%d",
			key, controller.Desired().Generation)
		return doRequestWithUserRetry(t, mux.ServeHTTP, "DELETE", path, "", user)
	}
	w = deleteKey("llm.api_key")
	if w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "delete llm.endpoint first") {
		t.Fatalf("DELETE = %d %s, want 400", w.Code, w.Body.String())
	}
	if v, ok := storedOverrideValue(t, cs, "llm.api_key"); !ok || v != "junk" {
		t.Fatalf("stored llm.api_key = %q (present=%v), want the override kept", v, ok)
	}
	if got := controller.Desired().Config.LLM; got.APIKey != "junk" ||
		got.Endpoint != "https://evil.example/v1" {
		t.Fatalf("desired config changed: endpoint %q", got.Endpoint)
	}
	if w = deleteKey("llm.endpoint"); w.Code != http.StatusOK {
		t.Fatalf("DELETE endpoint = %d %s, want 200", w.Code, w.Body.String())
	}
	w = deleteKey("llm.api_key")
	got := controller.Desired().Config.LLM
	if w.Code != http.StatusOK || got.APIKey != base.LLM.APIKey ||
		got.Endpoint != base.LLM.Endpoint {
		t.Fatalf("DELETE key after endpoint = %d %s, want the file pair back",
			w.Code, w.Body.String())
	}
}
