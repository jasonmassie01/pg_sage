# Pluggable LLM Provider Interface

## Design

pg_sage uses a pluggable LLM provider for Tier 2 features. The interface is intentionally minimal. Providers are selected at config time and wrapped with a circuit breaker.

## Provider Interface

```go
// internal/llm/provider.go

type CompletionRequest struct {
    SystemPrompt string
    Messages     []Message
    MaxTokens    int
    Temperature  float64
}

type Message struct {
    Role    string // "user", "assistant"
    Content string
}

type CompletionResponse struct {
    Content      string
    FinishReason string
    TokensUsed   int
}

type Provider interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
    Name() string
    Available(ctx context.Context) bool
}
```

## Provider Selection

```go
func NewProvider(cfg config.LLMConfig) (Provider, error) {
    switch cfg.Provider {
    case "openai":
        return NewOpenAIProvider(cfg.APIKey, cfg.Model, cfg.BaseURL)
    case "anthropic":
        return NewAnthropicProvider(cfg.APIKey, cfg.Model)
    case "ollama":
        return NewOllamaProvider(cfg.BaseURL, cfg.Model)
    default:
        return nil, fmt.Errorf("unknown LLM provider: %s", cfg.Provider)
    }
}
```

## OpenAI-Compatible Provider

Covers: OpenAI, Groq, Together, vLLM, LM Studio, Gemini (via OpenAI-compatible endpoint).

- POST to `{base_url}/v1/chat/completions`
- `base_url` defaults to `https://api.openai.com` but is configurable
- Use `net/http` with explicit timeout (30s default, configurable)
- Parse `choices[0].message.content` from response
- Handle streaming later if needed (initial impl: non-streaming)

## Circuit Breaker Wrapping

All providers are wrapped with a circuit breaker:

```go
type CircuitWrappedProvider struct {
    inner   Provider
    breaker *circuit.Breaker
}

func (p *CircuitWrappedProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
    if !p.breaker.Allow() {
        return CompletionResponse{}, ErrCircuitOpen
    }

    resp, err := p.inner.Complete(ctx, req)
    if err != nil {
        p.breaker.RecordFailure()
        return resp, err
    }

    p.breaker.RecordSuccess()
    return resp, nil
}
```

## Prompt Patterns for Tier 2

### Daily Briefing
System prompt sets context as a PostgreSQL DBA. User message includes structured findings from Tier 1 as JSON. Response is a human-readable health summary.

### Diagnose (ReAct Loop)
System prompt enables tool use: the LLM can request SQL queries to be executed. The sidecar executes queries and feeds results back. Loop terminates when LLM provides a final diagnosis or max iterations reached.

### Explain Narrative
Input: raw EXPLAIN ANALYZE output. Output: human-readable narrative of what the query does, why it's slow, and what to do about it.

## Testing LLM Providers

Mock the HTTP transport, not the provider:

```go
func TestOpenAIProvider_Complete(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        assert.Equal(t, "POST", r.Method)
        assert.Equal(t, "/v1/chat/completions", r.URL.Path)

        json.NewEncoder(w).Encode(map[string]interface{}{
            "choices": []map[string]interface{}{
                {"message": map[string]string{"content": "test response"}},
            },
        })
    }))
    defer server.Close()

    p, _ := NewOpenAIProvider("test-key", "gpt-4", server.URL)
    resp, err := p.Complete(context.Background(), CompletionRequest{
        Messages: []Message{{Role: "user", Content: "hello"}},
    })

    require.NoError(t, err)
    assert.Equal(t, "test response", resp.Content)
}
```
