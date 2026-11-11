package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/egress"
)

// Client is an OpenAI-compatible LLM client with circuit breaker.
type Client struct {
	cfg                 *config.LLMConfig
	httpClient          *http.Client
	logFn               func(string, string, ...any)
	authorizeDatabricks func(*http.Request) error
	stateMu             sync.Mutex
	generation          uint64
	inflightID          uint64
	inflight            map[uint64]context.CancelFunc

	// Circuit breaker state (breaker.go). failures counts consecutive
	// counted failures; lastClass, lastErrorAt and lastSuccessAt feed the
	// status. retryDelays is the ladder when no Retry-After is sent.
	mu            sync.Mutex
	state         breakerState
	failures      int
	openedAt      time.Time
	cooldown      time.Duration
	lastClass     errClass
	lastErrorAt   time.Time
	lastSuccessAt time.Time
	retryDelays   []time.Duration

	// Token budget tracking. Both fields are atomic because Chat is
	// called concurrently from per-database analyzers/optimizers/advisors
	// (C1); budgetResetDay was previously a plain int raced against the
	// atomic counter.
	tokensUsedToday atomic.Int64
	budgetResetDay  atomic.Int64
	budgetMu        sync.Mutex
	reservedTokens  int64

	// budget is an optional external token budget (e.g. a per-database
	// allocation in fleet mode), enforced in addition to the daily
	// budget. Set once before concurrent use via SetBudget; nil disables.
	budget Budgeter
	// ceiling is an optional stricter daily cap (SetCeiling) and global
	// the optional process-wide budget (SetGlobalBudget); both are atomic
	// because wiring may set them while other runtimes already call Chat.
	ceiling atomic.Pointer[ceilingFunc]
	global  atomic.Pointer[Budget]

	throttleMu sync.Mutex
	activeKeys map[string]struct{}
	lastCalls  map[string]time.Time

	// observer receives one Outcome per call (observer.go); nil is none.
	observer atomic.Pointer[func(Outcome)]
}

// Budgeter is an optional token budget supplied by the caller — used to
// give each database its own LLM allocation in fleet mode so one noisy
// DB can't drain the whole token budget (F5). nil disables it.
type Budgeter interface {
	CanSpend(tokens int) bool
	// Spend applies a usage delta. Negative deltas release a reservation
	// after a request fails or uses fewer tokens than estimated.
	Spend(tokens int)
}

// SetBudget attaches an external token budget. Call during wiring,
// before the client is used concurrently.
func (c *Client) SetBudget(b Budgeter) { c.budget = b }

type ChatRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	// MaxCompletionTokens replaces MaxTokens for models that reject it.
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
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
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// New creates a new LLM client.
func New(cfg *config.LLMConfig, logFn func(string, string, ...any)) *Client {
	if cfg == nil {
		cfg = &config.LLMConfig{}
	}
	snapshot := *cfg
	return &Client{
		cfg:                 &snapshot,
		httpClient:          egress.HTTPClient(0),
		logFn:               logFn,
		authorizeDatabricks: databricksauth.Authorize,
		cooldown:            circuitCooldown(cfg.CooldownSeconds),
		retryDelays:         defaultRetryDelays(),
		generation:          1,
		inflight:            make(map[uint64]context.CancelFunc),
		activeKeys:          make(map[string]struct{}),
		lastCalls:           make(map[string]time.Time),
	}
}

// IsEnabled returns true if LLM is configured and enabled.
func (c *Client) IsEnabled() bool {
	cfg, _ := c.configSnapshot()
	return configEnabled(cfg)
}

// Chat sends a chat completion request and returns the content and the
// tokens charged. It is ChatResult without the truncation signal; see
// there for empty and truncated answers.
func (c *Client) Chat(
	ctx context.Context,
	system string,
	user string,
	maxTokens int,
) (string, int, error) {
	r, err := c.ChatResult(ctx, system, user, maxTokens)
	return r.Content, r.Tokens, err
}

// checkAvailable admits a request: the client must be enabled and the
// breaker must let it through. probe is the half-open probe flag the
// caller passes to endProbe.
func (c *Client) checkAvailable(
	ctx context.Context, cfg config.LLMConfig,
) (probe bool, err error) {
	if !configEnabled(cfg) {
		return false, ErrLLMDisabled
	}
	return c.admit(forcedProbe(ctx))
}

// usageTokens returns provider-reported usage, estimating it when the
// provider omits usage so calls are never free (G3-B25).
func usageTokens(resp *ChatResponse, system, user string) int {
	if resp.Usage.TotalTokens > 0 {
		return resp.Usage.TotalTokens
	}
	return estimateTokens(system, user, resp.Choices[0].Message.Content)
}

// sendChat performs the HTTP exchange, adapting the request shape when
// the provider asks (wire_compat.go), and returns a decoded response with
// at least one choice. Provider failures are classified (errclass.go);
// the caller feeds them to the breaker once.
func (c *Client) sendChat(
	ctx, requestCtx context.Context,
	cfg config.LLMConfig,
	system, user string,
	maxTokens int,
) (*ChatResponse, error) {
	defer recordWait(ctx, time.Now())
	var out *ChatResponse
	err := c.withAdaptation(cfg, requestShape(cfg, false), func(shape wireShape) error {
		req := buildChatRequest(cfg, system, user, maxTokens, shape)
		resp, err := c.sendChatOnce(ctx, requestCtx, cfg, req, shape)
		out = resp
		return err
	})
	return out, err
}

func (c *Client) sendChatOnce(
	ctx, requestCtx context.Context, cfg config.LLMConfig, req ChatRequest,
	shape wireShape,
) (*ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	reply, err := c.post(ctx, requestCtx, cfg, body, maxAttempts, "LLM request")
	if err != nil {
		return nil, err
	}
	return c.decodeChatResponse(reply, cfg, shape)
}

func (c *Client) decodeChatResponse(
	reply providerReply, cfg config.LLMConfig, shape wireShape,
) (*ChatResponse, error) {
	if reply.status != http.StatusOK {
		return nil, c.statusError(cfg, shape, false, reply)
	}
	var chatResp ChatResponse
	if err := json.Unmarshal(reply.body, &chatResp); err != nil {
		return nil, malformed(fmt.Errorf("unmarshal: %w", err))
	}
	if len(chatResp.Choices) == 0 {
		return nil, malformed(fmt.Errorf("no choices in response"))
	}
	return &chatResp, nil
}

// buildChatRequest assembles the provider request. json_object mode is
// only requested when the system prompt asks for JSON (G3-B11): prose
// callers (briefing, narrators) must get prose, and OpenAI rejects
// json_object when no message mentions JSON. The shape names the cap.
func buildChatRequest(
	cfg config.LLMConfig, system, user string, maxTokens int, shape wireShape,
) ChatRequest {
	req := ChatRequest{
		Model: cfg.Model,
		Messages: []ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: maxTokens,
	}
	if shape.completionTokens {
		req.MaxTokens, req.MaxCompletionTokens = 0, maxTokens
	}
	if cfg.JSONMode && promptRequestsJSON(system) {
		req.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}
	return req
}

// promptRequestsJSON reports whether a (code-controlled) system prompt
// asks for JSON output. Only the system prompt is inspected so that
// database text in the user prompt cannot switch output modes.
func promptRequestsJSON(system string) bool {
	return strings.Contains(strings.ToLower(system), "json")
}

// chatEndpoint normalizes the configured endpoint to .../chat/completions,
// stripping a suffix that is already present to prevent double paths.
func chatEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	endpoint = strings.TrimSuffix(endpoint, "/chat")
	return endpoint + "/chat/completions"
}

func shouldRecordProviderFailure(
	callerCtx context.Context,
	requestCtx context.Context,
) bool {
	if requestCtx.Err() == nil {
		return true
	}
	if callerCtx != nil && callerCtx.Err() != nil {
		return false
	}
	return requestCtx.Err() == context.DeadlineExceeded
}

// doRequest keeps native workspace credentials on the request's validated
// origin. net/http's default redirect policy can forward Authorization to
// subdomains and other ports, so native requests must not follow redirects.
func (c *Client) doRequest(req *http.Request, cfg config.LLMConfig) (*http.Response, error) {
	if err := cfg.ValidateDatabricksApp(); err != nil {
		return nil, err
	}
	client := c.httpClient
	if cfg.Auth == "databricks" {
		nativeClient := *client
		nativeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &nativeClient
	}
	return client.Do(req)
}

// NewOptimizerClient creates an independent LLM client for the optimizer.
// It inherits endpoint and model from the general LLM config when the
// optimizer-specific fields are empty; the API key follows OptimizerAPIKey.
func NewOptimizerClient(
	parent *config.LLMConfig,
	opt *config.OptimizerLLMConfig,
	logFn func(string, string, ...any),
) *Client {
	authConfig := *parent
	authConfig.OptimizerLLM = *opt
	endpoint := opt.Endpoint
	if endpoint == "" {
		endpoint = parent.Endpoint
	}
	apiKey := OptimizerAPIKey(authConfig)
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
		Auth:             authConfig.OptimizerAuth(),
		Enabled:          opt.Enabled,
		Endpoint:         endpoint,
		APIKey:           apiKey,
		Model:            model,
		TimeoutSeconds:   timeout,
		TokenBudgetDaily: budget,
		CooldownSeconds:  cooldown,
		JSONMode:         parent.JSONMode,
		// Wire overrides describe the provider, like json_mode.
		TokenParameter:      parent.TokenParameter,
		ToolReasoningEffort: parent.ToolReasoningEffort,
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

// IsBudgetExhausted returns true if the effective daily token budget
// (EffectiveTokenBudgetDaily) has been reached. Returns false when no
// budget applies.
func (c *Client) IsBudgetExhausted() bool {
	cfg, _ := c.configSnapshot()
	capacity := c.dailyLimit(cfg)
	if !capacity.limited {
		return false
	}
	used := c.tokensUsedToday.Load()
	if budgetDay(time.Now()) != c.budgetResetDay.Load() {
		used = 0
	}
	return used >= capacity.limit
}

// ResetBudget zeroes the daily token counter, allowing LLM calls
// to resume immediately instead of waiting for the next calendar day.
// Holds of calls in flight stay, so their settle cannot go negative.
func (c *Client) ResetBudget() {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.tokensUsedToday.Store(0)
	c.budgetResetDay.Store(budgetDay(time.Now()))
}

// Model returns the configured model name.
func (c *Client) Model() string {
	cfg, _ := c.configSnapshot()
	return cfg.Model
}
