package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func TestListModelsHandler_NotConfigured(t *testing.T) {
	cfg := &config.LLMConfig{}
	handler := listModelsHandler(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if !strings.Contains(body["error"], "not configured") {
		t.Errorf("error should mention not configured, got: %s",
			body["error"])
	}
}

func TestListModelsHandler_MissingAPIKey(t *testing.T) {
	cfg := &config.LLMConfig{
		Endpoint: "https://example.com/v1",
	}
	handler := listModelsHandler(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestListModelsHandler_MissingEndpoint(t *testing.T) {
	cfg := &config.LLMConfig{
		APIKey: "some-key",
	}
	handler := listModelsHandler(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestListModelsHandler_Success(t *testing.T) {
	// Set up a mock server that returns models.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[
				{"id":"gpt-4o","object":"model","owned_by":"openai"}
			]}`))
		}))
	defer srv.Close()

	// Clear the model cache so we hit the mock server.
	llm.InvalidateModelCache()

	cfg := &config.LLMConfig{
		Endpoint: srv.URL,
		APIKey:   "test-key",
		Model:    "gpt-4o",
	}
	handler := listModelsHandler(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Models  []llm.ModelInfo `json:"models"`
		Current string          `json:"current"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Current != "gpt-4o" {
		t.Errorf("expected current gpt-4o, got %s", body.Current)
	}
	if len(body.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(body.Models))
	}
	if body.Models[0].ID != "gpt-4o" {
		t.Errorf("expected model id gpt-4o, got %s", body.Models[0].ID)
	}
}

func TestListModelsHandler_ProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limited", 429)
		}))
	defer srv.Close()

	llm.InvalidateModelCache()

	cfg := &config.LLMConfig{
		Endpoint: srv.URL,
		APIKey:   "test-key",
		Model:    "gpt-4o",
	}
	handler := listModelsHandler(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}
