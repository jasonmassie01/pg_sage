package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Client is an OpenAI-compatible LLM client with circuit breaker.
type Client struct {
	cfg        *config.LLMConfig
	httpClient *http.Client
	logFn      func(string, string, ...any)

	// Circuit breaker state.
	mu            sync.Mutex
	failures      int
	circuitOpen   bool
	circuitOpened time.Time
	cooldown      time.Duration

	// Token budget tracking.
	tokensUsedToday atomic.Int64
	budgetResetDay  int
}

type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat is the OpenAI-style structured-output hint. Only
// the "json_object" type is used by pg_sage; providers that don't
// support it typically ignore unknown fields. We keep it omitempty
// so requests without JSON mode are byte-identical to the old shape.
type ResponseFormat struct {
	Type string `json:"type"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// New creates a new LLM client.
func New(cfg *config.LLMConfig, logFn func(string, string, ...any)) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
		logFn:    logFn,
		cooldown: time.Duration(cfg.CooldownSeconds) * time.Second,
	}
}

// IsEnabled returns true if LLM is configured and enabled.
func (c *Client) IsEnabled() bool {
	return c.cfg.Enabled && c.cfg.Endpoint != "" && c.cfg.APIKey != ""
}

// IsCircuitOpen returns true if the circuit breaker is open.
func (c *Client) IsCircuitOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.circuitOpen {
		return false
	}
	if time.Since(c.circuitOpened) > c.cooldown {
		c.circuitOpen = false
		c.failures = 0
		c.logFn("llm", "circuit breaker closed (cooldown expired)")
		return false
	}
	return true
}

// Chat sends a chat completion request.
func (c *Client) Chat(ctx context.Context, system, user string, maxTokens int) (string, int, error) {
	if !c.IsEnabled() {
		return "", 0, fmt.Errorf("LLM not enabled")
	}
	if c.IsCircuitOpen() {
		return "", 0, fmt.Errorf("LLM circuit breaker open")
	}

	// Check token budget.
	today := time.Now().YearDay()
	if today != c.budgetResetDay {
		c.tokensUsedToday.Store(0)
		c.budgetResetDay = today
	}
	if c.cfg.TokenBudgetDaily > 0 &&
		int(c.tokensUsedToday.Load()) >= c.cfg.TokenBudgetDaily {
		return "", 0, fmt.Errorf("daily token budget exhausted (%d/%d)",
			c.tokensUsedToday.Load(), c.cfg.TokenBudgetDaily)
	}

	// Thinking models (Gemini 2.5 Flash/Pro) consume output budget for
	// internal reasoning. Add a fixed overhead so thinking tokens
	// don't crowd out the actual response content.
	if maxTokens <= 0 {
		maxTokens = 16384
	} else if isThinkingModel(c.cfg.Model) {
		maxTokens += 16384
	}

	req := ChatRequest{
		Model: c.cfg.Model,
		Messages: []ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: maxTokens,
	}
	if c.cfg.JSONMode {
		req.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", 0, fmt.Errorf("marshal: %w", err)
	}

	endpoint := c.cfg.Endpoint
	// Strip trailing /chat/completions if already present to prevent
	// double-path (e.g. .../v1/chat/completions/chat/completions).
	endpoint = strings.TrimRight(endpoint, "/")
	endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	endpoint = strings.TrimSuffix(endpoint, "/chat")
	endpoint += "/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.doWithRetry(ctx, httpReq, body)
	if err != nil {
		c.recordFailure()
		return "", 0, err
	}
	defer resp.Body.Close()

	// Cap response body at 1MB to prevent memory exhaustion.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.recordFailure()
		return "", 0, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		c.recordFailure()
		return "", 0, fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", 0, fmt.Errorf("unmarshal: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", 0, fmt.Errorf("no choices in response")
	}

	c.recordSuccess()
	tokens := chatResp.Usage.TotalTokens
	c.tokensUsedToday.Add(int64(tokens))

	content := chatResp.Choices[0].Message.Content
	reason := chatResp.Choices[0].FinishReason
	if reason == "length" || reason == "max_tokens" {
		c.logFn("llm",
			"response truncated (finish_reason=%s, tokens=%d), "+
				"attempting JSON repair", reason, tokens)
		content = RepairTruncatedJSON(content)
	}

	return content, tokens, nil
}

func (c *Client) doWithRetry(ctx context.Context, req *http.Request, body []byte) (*http.Response, error) {
	delays := []time.Duration{1 * time.Second, 4 * time.Second, 16 * time.Second}
	var lastErr error

	for i := 0; i <= len(delays); i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delays[i-1]):
			}
			// Recreate request with fresh body.
			var err error
			req, err = http.NewRequestWithContext(ctx, req.Method, req.URL.String(), bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server error %d", resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures >= 3 {
		c.circuitOpen = true
		c.circuitOpened = time.Now()
		c.logFn("llm", "circuit breaker opened after %d failures", c.failures)
	}
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
}

// NewOptimizerClient creates an independent LLM client for the optimizer.
// It inherits endpoint, API key, and model from the general LLM config
// when the optimizer-specific fields are empty.
func NewOptimizerClient(
	parent *config.LLMConfig,
	opt *config.OptimizerLLMConfig,
	logFn func(string, string, ...any),
) *Client {
	endpoint := opt.Endpoint
	if endpoint == "" {
		endpoint = parent.Endpoint
	}
	apiKey := opt.APIKey
	if apiKey == "" {
		apiKey = parent.APIKey
	}
	model := opt.Model
	if model == "" {
		model = parent.Model
	}
	timeout := opt.TimeoutSeconds
	if timeout <= 0 {
		timeout = parent.TimeoutSeconds
	}
	budget := opt.TokenBudgetDaily
	if budget <= 0 {
		budget = parent.TokenBudgetDaily
	}
	cooldown := opt.CooldownSeconds
	if cooldown <= 0 {
		cooldown = parent.CooldownSeconds
	}

	merged := &config.LLMConfig{
		Enabled:          opt.Enabled,
		Endpoint:         endpoint,
		APIKey:           apiKey,
		Model:            model,
		TimeoutSeconds:   timeout,
		TokenBudgetDaily: budget,
		CooldownSeconds:  cooldown,
		JSONMode:         parent.JSONMode,
	}
	return New(merged, logFn)
}

// TokensUsedToday returns the number of tokens used today.
func (c *Client) TokensUsedToday() int64 {
	return c.tokensUsedToday.Load()
}

// TokenBudgetDaily returns the configured daily token budget.
// Returns 0 if no budget is configured (unlimited).
func (c *Client) TokenBudgetDaily() int {
	return c.cfg.TokenBudgetDaily
}

// IsBudgetExhausted returns true if the daily token budget has
// been reached. Returns false when no budget is configured.
func (c *Client) IsBudgetExhausted() bool {
	if c.cfg.TokenBudgetDaily <= 0 {
		return false
	}
	today := time.Now().YearDay()
	if today != c.budgetResetDay {
		return false
	}
	return int(c.tokensUsedToday.Load()) >= c.cfg.TokenBudgetDaily
}

// ResetBudget zeroes the daily token counter, allowing LLM calls
// to resume immediately instead of waiting for the next calendar day.
func (c *Client) ResetBudget() {
	c.tokensUsedToday.Store(0)
	c.budgetResetDay = time.Now().YearDay()
}

// Model returns the configured model name.
func (c *Client) Model() string {
	return c.cfg.Model
}
