package llm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ModelInfo describes an LLM model returned by the provider.
type ModelInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	InputTokenLimit  int    `json:"input_token_limit,omitempty"`
	OutputTokenLimit int    `json:"output_token_limit,omitempty"`
	Description      string `json:"description,omitempty"`
}

// modelCache stores cached model listings with TTL.
type modelCache struct {
	mu      sync.Mutex
	entries map[string]modelCacheEntry
	ttl     time.Duration
}

type modelCacheEntry struct {
	models  []ModelInfo
	fetched time.Time
}

// defaultCache is the package-level cache (1-hour TTL).
var defaultCache = &modelCache{
	entries: make(map[string]modelCacheEntry),
	ttl:     time.Hour,
}

func (c *modelCache) get() ([]ModelInfo, bool) {
	return c.getFor("")
}

func (c *modelCache) getFor(key string) ([]ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if ok && entry.models != nil && time.Since(entry.fetched) < c.ttl {
		return append([]ModelInfo(nil), entry.models...), true
	}
	return nil, false
}

func (c *modelCache) set(models []ModelInfo) {
	c.setFor("", models)
}

func (c *modelCache) setFor(key string, models []ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]modelCacheEntry)
	}
	c.entries[key] = modelCacheEntry{
		models: append([]ModelInfo(nil), models...), fetched: time.Now(),
	}
}

// ListModels queries the LLM provider for available models.
// Results are cached for 1 hour.
func ListModels(
	ctx context.Context, endpoint, apiKey string,
) ([]ModelInfo, error) {
	return ListModelsWithClient(
		ctx, endpoint, apiKey, http.DefaultClient,
	)
}

// ListModelsWithClient queries models with a caller-controlled HTTP client.
// Security-sensitive callers use this to pin validated DNS resolutions.
func ListModelsWithClient(
	ctx context.Context,
	endpoint string,
	apiKey string,
	httpClient *http.Client,
) ([]ModelInfo, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("HTTP client is required")
	}
	key := modelCacheKey(endpoint, apiKey)
	if cached, ok := defaultCache.getFor(key); ok {
		return cached, nil
	}
	models, err := fetchModelsWithClient(
		ctx, endpoint, apiKey, httpClient,
	)
	if err != nil {
		return nil, err
	}
	defaultCache.setFor(key, models)
	return models, nil
}

// InvalidateModelCache clears the cached model list.
func InvalidateModelCache() {
	defaultCache.mu.Lock()
	defer defaultCache.mu.Unlock()
	defaultCache.entries = make(map[string]modelCacheEntry)
}

func modelCacheKey(endpoint, apiKey string) string {
	digest := sha256.Sum256([]byte(apiKey))
	return strings.TrimRight(strings.TrimSpace(endpoint), "/") + "|" +
		fmt.Sprintf("%x", digest[:16])
}

func fetchModelsWithClient(
	ctx context.Context,
	endpoint string,
	apiKey string,
	httpClient *http.Client,
) ([]ModelInfo, error) {
	if isGeminiEndpoint(endpoint) {
		return fetchGeminiModelsWithClient(ctx, apiKey, httpClient)
	}
	return fetchOpenAIModelsWithClient(
		ctx, endpoint, apiKey, httpClient,
	)
}

func isGeminiEndpoint(endpoint string) bool {
	return strings.Contains(
		endpoint, "generativelanguage.googleapis.com")
}

func fetchGeminiModelsWithClient(
	ctx context.Context,
	apiKey string,
	httpClient *http.Client,
) ([]ModelInfo, error) {
	url := "https://generativelanguage.googleapis.com/" +
		"v1beta/models?key=" + apiKey
	body, err := doModelRequestWithClient(ctx, url, "", httpClient)
	if err != nil {
		return nil, fmt.Errorf("gemini list models: %w", err)
	}
	return parseGeminiModels(body)
}

// geminiListResponse is the Gemini API response shape.
type geminiListResponse struct {
	Models []geminiModel `json:"models"`
}

type geminiModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

func parseGeminiModels(data []byte) ([]ModelInfo, error) {
	var resp geminiListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}
	var models []ModelInfo
	for _, m := range resp.Models {
		if !supportsGenerate(m.SupportedGenerationMethods) {
			continue
		}
		models = append(models, ModelInfo{
			ID:               stripModelsPrefix(m.Name),
			Name:             m.DisplayName,
			InputTokenLimit:  m.InputTokenLimit,
			OutputTokenLimit: m.OutputTokenLimit,
			Description:      m.Description,
		})
	}
	return models, nil
}

func supportsGenerate(methods []string) bool {
	for _, m := range methods {
		if m == "generateContent" {
			return true
		}
	}
	return false
}

func stripModelsPrefix(name string) string {
	return strings.TrimPrefix(name, "models/")
}

// fetchOpenAIModels calls the OpenAI-compatible /models endpoint.
func fetchOpenAIModels(
	ctx context.Context, endpoint, apiKey string,
) ([]ModelInfo, error) {
	return fetchOpenAIModelsWithClient(
		ctx, endpoint, apiKey, http.DefaultClient,
	)
}

func fetchOpenAIModelsWithClient(
	ctx context.Context,
	endpoint string,
	apiKey string,
	httpClient *http.Client,
) ([]ModelInfo, error) {
	base := strings.TrimRight(endpoint, "/")
	// Strip chat/completions suffixes to get the base URL.
	base = strings.TrimSuffix(base, "/chat/completions")
	base = strings.TrimSuffix(base, "/chat")
	url := base + "/models"
	body, err := doModelRequestWithClient(ctx, url, apiKey, httpClient)
	if err != nil {
		return nil, fmt.Errorf("openai list models: %w", err)
	}
	return parseOpenAIModels(body)
}

// openAIListResponse is the OpenAI /models response shape.
type openAIListResponse struct {
	Data []openAIModel `json:"data"`
}

type openAIModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

func parseOpenAIModels(data []byte) ([]ModelInfo, error) {
	var resp openAIListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	models := make([]ModelInfo, 0, len(resp.Data))
	for _, m := range resp.Data {
		models = append(models, ModelInfo{
			ID:   m.ID,
			Name: m.ID,
		})
	}
	return models, nil
}

// doModelRequest performs a GET with a 10s timeout.
func doModelRequest(
	ctx context.Context, url, apiKey string,
) ([]byte, error) {
	return doModelRequestWithClient(
		ctx, url, apiKey, http.DefaultClient,
	)
}

func doModelRequestWithClient(
	ctx context.Context,
	url string,
	apiKey string,
	httpClient *http.Client,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, providerRequestError("create request", err)
	}
	if apiKey != "" {
		req.Header.Set(
			"Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, providerRequestError("http request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		redactedBody := redactProviderText(string(body))
		slog.Warn("model list API error",
			"status", resp.StatusCode,
			"body", redactedBody)
		return nil, fmt.Errorf(
			"API returned %d: %s",
			resp.StatusCode, truncate(redactedBody, 200))
	}
	return body, nil
}

func providerRequestError(prefix string, err error) error {
	return &redactedProviderError{prefix: prefix, cause: err}
}

type redactedProviderError struct {
	prefix string
	cause  error
}

func (e *redactedProviderError) Error() string {
	return fmt.Sprintf("%s: %s", e.prefix, redactProviderText(e.cause.Error()))
}

func (e *redactedProviderError) Unwrap() error { return e.cause }

func redactProviderText(value string) string {
	for _, key := range []string{
		"key", "api_key", "access_token", "token", "signature",
	} {
		value = redactQueryValue(value, key)
	}
	return value
}

func redactQueryValue(value, key string) string {
	lower := strings.ToLower(value)
	needle := strings.ToLower(key) + "="
	for start := 0; ; {
		index := strings.Index(lower[start:], needle)
		if index < 0 {
			return value
		}
		index += start
		valueStart := index + len(needle)
		valueEnd := len(value)
		for i := valueStart; i < len(value); i++ {
			if strings.ContainsRune("& #\"'", rune(value[i])) {
				valueEnd = i
				break
			}
		}
		value = value[:valueStart] + "[REDACTED]" + value[valueEnd:]
		lower = strings.ToLower(value)
		start = valueStart + len("[REDACTED]")
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
