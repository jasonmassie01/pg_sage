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
	stateMu    sync.Mutex
	generation uint64
	inflightID uint64
	inflight   map[uint64]context.CancelFunc

	// Circuit breaker state.
	mu            sync.Mutex
	failures      int
	circuitOpen   bool
	circuitOpened time.Time
	cooldown      time.Duration

	// Token budget tracking. Both fields are atomic because Chat is
	// called concurrently from per-database analyzers/optimizers/advisors
	// (C1); budgetResetDay was previously a plain int raced against the
	// atomic counter.
	tokensUsedToday  atomic.Int64
	budgetResetDay   atomic.Int64
	budgetMu         sync.Mutex
	reservedTokens   int64
	reservedExternal int

	// budget is an optional external token budget (e.g. a per-database
	// allocation in fleet mode), enforced in addition to the daily
	// budget. Set once before concurrent use via SetBudget; nil disables.
	budget Budgeter

	throttleMu sync.Mutex
	activeKeys map[string]struct{}
	lastCalls  map[string]time.Time
}

// Budgeter is an optional token budget supplied by the caller — used to
// give each database its own LLM allocation in fleet mode so one noisy
// DB can't drain the whole token budget (F5). nil disables it.
type Budgeter interface {
	CanSpend(tokens int) bool
	Spend(tokens int)
}

// SetBudget attaches an external token budget. Call during wiring,
// before the client is used concurrently.
func (c *Client) SetBudget(b Budgeter) { c.budget = b }

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
	if cfg == nil {
		cfg = &config.LLMConfig{}
	}
	snapshot := *cfg
	return &Client{
		cfg:        &snapshot,
		httpClient: &http.Client{},
		logFn:      logFn,
		cooldown:   circuitCooldown(cfg.CooldownSeconds),
		generation: 1,
		inflight:   make(map[uint64]context.CancelFunc),
		activeKeys: make(map[string]struct{}),
		lastCalls:  make(map[string]time.Time),
	}
}

// IsEnabled returns true if LLM is configured and enabled.
func (c *Client) IsEnabled() bool {
	cfg, _ := c.configSnapshot()
	return configEnabled(cfg)
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
	requestCtx, cfg, generation, finish := c.beginRequest(ctx)
	defer finish()
	if !configEnabled(cfg) {
		return "", 0, fmt.Errorf("LLM not enabled")
	}
	if c.IsCircuitOpen() {
		return "", 0, fmt.Errorf("LLM circuit breaker open")
	}
	maxTokens = normalizedMaxTokens(cfg.Model, maxTokens)
	throttleKey := requestThrottleKey(cfg)
	if err := c.acquireThrottle(throttleKey, cfg.CooldownSeconds); err != nil {
		return "", 0, err
	}
	throttleSuccess := false
	defer func() { c.releaseThrottle(throttleKey, throttleSuccess) }()
	reservation, err := c.reserveBudget(cfg, maxTokens)
	if err != nil {
		return "", 0, err
	}
	reconciled := false
	defer func() {
		if !reconciled {
			c.releaseBudget(reservation)
		}
	}()

	req := ChatRequest{
		Model: cfg.Model,
		Messages: []ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: maxTokens,
	}
	if cfg.JSONMode {
		req.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", 0, fmt.Errorf("marshal: %w", err)
	}

	endpoint := cfg.Endpoint
	// Strip trailing /chat/completions if already present to prevent
	// double-path (e.g. .../v1/chat/completions/chat/completions).
	endpoint = strings.TrimRight(endpoint, "/")
	endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	endpoint = strings.TrimSuffix(endpoint, "/chat")
	endpoint += "/chat/completions"

	httpReq, err := http.NewRequestWithContext(
		requestCtx, "POST", endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return "", 0, fmt.Errorf("request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := c.doWithRetry(requestCtx, httpReq, body, cfg.APIKey)
	if err != nil {
		if requestCtx.Err() == nil {
			c.recordFailure()
		}
		return "", 0, providerRequestError("LLM request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap response body at 1MB to prevent memory exhaustion.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.recordFailure()
		return "", 0, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		c.recordFailure()
		return "", 0, fmt.Errorf(
			"LLM API error %d: %s",
			resp.StatusCode,
			redactProviderText(string(respBody)),
		)
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		c.recordFailure()
		return "", 0, fmt.Errorf("unmarshal: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		c.recordFailure()
		return "", 0, fmt.Errorf("no choices in response")
	}
	if !c.generationCurrent(generation) {
		return "", 0, fmt.Errorf("LLM disabled or reconfigured during request")
	}

	c.recordSuccess()
	tokens := chatResp.Usage.TotalTokens
	c.reconcileBudget(reservation, tokens)
	reconciled = true
	throttleSuccess = true

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

func (c *Client) doWithRetry(
	ctx context.Context,
	req *http.Request,
	body []byte,
	apiKey string,
) (*http.Response, error) {
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
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			_ = resp.Body.Close()
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
	cfg, _ := c.configSnapshot()
	return cfg.TokenBudgetDaily
}

// IsBudgetExhausted returns true if the daily token budget has
// been reached. Returns false when no budget is configured.
func (c *Client) IsBudgetExhausted() bool {
	cfg, _ := c.configSnapshot()
	if cfg.TokenBudgetDaily <= 0 {
		return false
	}
	today := int64(time.Now().YearDay())
	if today != c.budgetResetDay.Load() {
		return false
	}
	return int(c.tokensUsedToday.Load()) >= cfg.TokenBudgetDaily
}

// ResetBudget zeroes the daily token counter, allowing LLM calls
// to resume immediately instead of waiting for the next calendar day.
func (c *Client) ResetBudget() {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.tokensUsedToday.Store(0)
	c.reservedTokens = 0
	c.reservedExternal = 0
	c.budgetResetDay.Store(int64(time.Now().YearDay()))
}

// Model returns the configured model name.
func (c *Client) Model() string {
	cfg, _ := c.configSnapshot()
	return cfg.Model
}
