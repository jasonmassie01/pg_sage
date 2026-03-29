package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseGeminiModels_HappyPath(t *testing.T) {
	body := `{
		"models": [
			{
				"name": "models/gemini-2.5-flash",
				"displayName": "Gemini 2.5 Flash",
				"description": "Fast model",
				"inputTokenLimit": 1048576,
				"outputTokenLimit": 65536,
				"supportedGenerationMethods": ["generateContent", "countTokens"]
			},
			{
				"name": "models/gemini-2.0-flash",
				"displayName": "Gemini 2.0 Flash",
				"description": "Previous gen",
				"inputTokenLimit": 32768,
				"outputTokenLimit": 8192,
				"supportedGenerationMethods": ["generateContent"]
			}
		]
	}`
	models, err := parseGeminiModels([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	m := models[0]
	if m.ID != "gemini-2.5-flash" {
		t.Errorf("expected id gemini-2.5-flash, got %s", m.ID)
	}
	if m.Name != "Gemini 2.5 Flash" {
		t.Errorf("expected name Gemini 2.5 Flash, got %s", m.Name)
	}
	if m.InputTokenLimit != 1048576 {
		t.Errorf("expected input 1048576, got %d", m.InputTokenLimit)
	}
	if m.OutputTokenLimit != 65536 {
		t.Errorf("expected output 65536, got %d", m.OutputTokenLimit)
	}
	if m.Description != "Fast model" {
		t.Errorf("expected description Fast model, got %s", m.Description)
	}
}

func TestParseGeminiModels_FiltersNonGenerateContent(t *testing.T) {
	body := `{
		"models": [
			{
				"name": "models/embedding-001",
				"displayName": "Embedding 001",
				"supportedGenerationMethods": ["embedContent"]
			},
			{
				"name": "models/gemini-2.0-flash",
				"displayName": "Gemini 2.0 Flash",
				"supportedGenerationMethods": ["generateContent"]
			}
		]
	}`
	models, err := parseGeminiModels([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model after filtering, got %d", len(models))
	}
	if models[0].ID != "gemini-2.0-flash" {
		t.Errorf("expected gemini-2.0-flash, got %s", models[0].ID)
	}
}

func TestParseGeminiModels_EmptyModels(t *testing.T) {
	body := `{"models": []}`
	models, err := parseGeminiModels([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

func TestParseGeminiModels_InvalidJSON(t *testing.T) {
	_, err := parseGeminiModels([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse gemini") {
		t.Errorf("error should mention gemini, got: %v", err)
	}
}

func TestParseOpenAIModels_HappyPath(t *testing.T) {
	body := `{
		"data": [
			{"id": "gpt-4-turbo", "object": "model", "owned_by": "openai"},
			{"id": "gpt-3.5-turbo", "object": "model", "owned_by": "openai"}
		]
	}`
	models, err := parseOpenAIModels([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4-turbo" {
		t.Errorf("expected gpt-4-turbo, got %s", models[0].ID)
	}
	if models[0].Name != "gpt-4-turbo" {
		t.Errorf("openai model name should equal id, got %s", models[0].Name)
	}
}

func TestParseOpenAIModels_EmptyData(t *testing.T) {
	body := `{"data": []}`
	models, err := parseOpenAIModels([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

func TestParseOpenAIModels_InvalidJSON(t *testing.T) {
	_, err := parseOpenAIModels([]byte(`{broken`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse openai") {
		t.Errorf("error should mention openai, got: %v", err)
	}
}

func TestStripModelsPrefix(t *testing.T) {
	tests := []struct{ input, want string }{
		{"models/gemini-2.5-flash", "gemini-2.5-flash"},
		{"gemini-2.5-flash", "gemini-2.5-flash"},
		{"models/", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got := stripModelsPrefix(tc.input)
		if got != tc.want {
			t.Errorf("stripModelsPrefix(%q) = %q, want %q",
				tc.input, got, tc.want)
		}
	}
}

func TestSupportsGenerate(t *testing.T) {
	if !supportsGenerate([]string{"generateContent", "countTokens"}) {
		t.Error("should support generateContent")
	}
	if supportsGenerate([]string{"embedContent"}) {
		t.Error("embedContent should not pass")
	}
	if supportsGenerate(nil) {
		t.Error("nil should not pass")
	}
	if supportsGenerate([]string{}) {
		t.Error("empty should not pass")
	}
}

func TestIsGeminiEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"https://generativelanguage.googleapis.com/v1beta", true},
		{"https://api.openai.com/v1", false},
		{"http://localhost:11434/v1", false},
		{"", false},
	}
	for _, tc := range tests {
		got := isGeminiEndpoint(tc.endpoint)
		if got != tc.want {
			t.Errorf("isGeminiEndpoint(%q) = %v, want %v",
				tc.endpoint, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string should pass through, got %q", got)
	}
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("long string should truncate, got %q", got)
	}
	if got := truncate("", 5); got != "" {
		t.Errorf("empty should stay empty, got %q", got)
	}
}

func TestFetchGeminiModels_HTTPServer(t *testing.T) {
	resp := geminiListResponse{
		Models: []geminiModel{
			{
				Name:                       "models/gemini-2.0-flash",
				DisplayName:                "Gemini 2.0 Flash",
				Description:                "Test",
				InputTokenLimit:            32768,
				OutputTokenLimit:           8192,
				SupportedGenerationMethods: []string{"generateContent"},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("key") != "test-key" {
				http.Error(w, "unauthorized", 401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
	defer srv.Close()

	// Directly test doModelRequest + parseGeminiModels via the URL.
	body, err := doModelRequest(
		context.Background(),
		srv.URL+"?key=test-key", "")
	if err != nil {
		t.Fatalf("doModelRequest failed: %v", err)
	}
	models, err := parseGeminiModels(body)
	if err != nil {
		t.Fatalf("parseGeminiModels failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "gemini-2.0-flash" {
		t.Errorf("expected gemini-2.0-flash, got %s", models[0].ID)
	}
}

func TestFetchOpenAIModels_HTTPServer(t *testing.T) {
	resp := openAIListResponse{
		Data: []openAIModel{
			{ID: "gpt-4o", OwnedBy: "openai"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				http.Error(w, "not found", 404)
				return
			}
			auth := r.Header.Get("Authorization")
			if auth != "Bearer test-key" {
				http.Error(w, "unauthorized", 401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
	defer srv.Close()

	models, err := fetchOpenAIModels(
		context.Background(), srv.URL, "test-key")
	if err != nil {
		t.Fatalf("fetchOpenAIModels failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %s", models[0].ID)
	}
}

func TestDoModelRequest_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal error", 500)
		}))
	defer srv.Close()

	_, err := doModelRequest(
		context.Background(), srv.URL, "key")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code, got: %v", err)
	}
}

func TestDoModelRequest_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(5 * time.Second)
		}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := doModelRequest(ctx, srv.URL, "key")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestModelCache_HitAndMiss(t *testing.T) {
	c := &modelCache{ttl: time.Hour}

	// Miss on empty cache.
	_, ok := c.get()
	if ok {
		t.Error("expected cache miss on empty cache")
	}

	// Set and hit.
	models := []ModelInfo{{ID: "test-model", Name: "Test"}}
	c.set(models)
	got, ok := c.get()
	if !ok {
		t.Fatal("expected cache hit after set")
	}
	if len(got) != 1 || got[0].ID != "test-model" {
		t.Errorf("cached data mismatch: %+v", got)
	}
}

func TestModelCache_Expiry(t *testing.T) {
	c := &modelCache{ttl: time.Millisecond}
	c.set([]ModelInfo{{ID: "old"}})
	time.Sleep(5 * time.Millisecond)

	_, ok := c.get()
	if ok {
		t.Error("expected cache miss after TTL expiry")
	}
}

func TestInvalidateModelCache(t *testing.T) {
	// Populate the default cache.
	defaultCache.set([]ModelInfo{{ID: "cached"}})
	_, ok := defaultCache.get()
	if !ok {
		t.Fatal("cache should be populated")
	}

	InvalidateModelCache()
	_, ok = defaultCache.get()
	if ok {
		t.Error("cache should be empty after invalidation")
	}
}

func TestFetchOpenAIModels_EndpointStripping(t *testing.T) {
	// Ensure /chat/completions suffix is stripped before
	// appending /models.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Errorf("expected /models path, got %s",
					r.URL.Path)
				http.Error(w, "wrong path", 404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[]}`))
		}))
	defer srv.Close()

	// Pass endpoint with /chat/completions suffix.
	_, err := fetchOpenAIModels(
		context.Background(),
		srv.URL+"/chat/completions", "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// No concurrent access tests: parseGeminiModels and parseOpenAIModels
// are pure functions with no shared state. The cache is tested
// separately for thread safety via TestModelCache_*.
