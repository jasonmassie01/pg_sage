package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-05: the pre-flight size guard refuses a prompt the model must
// reject, before the throttle, the budgets or any I/O, and only when the
// model's input limit is known. It never counts toward the breaker and
// never reads llm.context_budget_tokens.

// knownLimit records limit as the input limit of model under the
// identity of c's endpoint and key, as a model listing would.
func knownLimit(t *testing.T, c *Client, model string, limit int) {
	t.Helper()
	cfg, _ := c.configSnapshot()
	inputLimits.record(modelCacheKey(cfg.Endpoint, cfg.APIKey),
		[]ModelInfo{{ID: model, InputTokenLimit: limit}})
}

// promptOf returns a user prompt whose estimate is exactly tokens
// (estimateTokens is len/4 + 1).
func promptOf(tokens int) string {
	return strings.Repeat("a", (tokens-1)*4)
}

func TestPreflight_BoundaryIsLimitMinusMaxTokens(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 10))
	c := toolClient(ts.srv.URL)
	knownLimit(t, c, "m", 1000)
	if got := estimateTokens("", promptOf(800)); got != 800 {
		t.Fatalf("promptOf(800) estimates %d tokens", got)
	}
	if _, _, err := c.Chat(context.Background(), "", promptOf(800), 200); err != nil {
		t.Fatalf("prompt at limit - max_tokens refused: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the prompt at the bound sent", ts.calls.Load())
	}
	_, _, err := c.Chat(context.Background(), "", promptOf(801), 200)
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want ErrPromptTooLarge one token over", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d: a refused prompt reached the provider", ts.calls.Load())
	}
	for _, want := range []string{`"m"`, "801", "1000", "200", "not sent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestPreflight_UnknownLimitMeansNoGuard(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 10))
	c := toolClient(ts.srv.URL)
	if _, _, err := c.Chat(context.Background(), "", promptOf(100000), 100); err != nil {
		t.Fatalf("no known limit, yet refused: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", ts.calls.Load())
	}
}

func TestPreflight_LimitAppliesOnlyToItsModelAndIdentity(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 10))
	c := toolClient(ts.srv.URL)
	knownLimit(t, c, "other-model", 10)
	inputLimits.record(modelCacheKey(ts.srv.URL, "another-key"),
		[]ModelInfo{{ID: "m", InputTokenLimit: 10}})
	if _, _, err := c.Chat(context.Background(), "", promptOf(5000), 100); err != nil {
		t.Fatalf("a limit of another model or key guarded this one: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", ts.calls.Load())
	}
}

func TestPreflight_RefusalsLeaveBreakerAndBudgetUntouched(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 10))
	c := toolClient(ts.srv.URL)
	knownLimit(t, c, "m", 500)
	for i := 0; i < 5; i++ {
		_, _, err := c.Chat(context.Background(), "s", promptOf(2000+i), 100)
		if !errors.Is(err, ErrPromptTooLarge) {
			t.Fatalf("refusal %d: err = %v", i, err)
		}
	}
	st := StatusOf(c)
	if st.ConsecutiveFailures != 0 || st.CircuitState != "closed" || st.CircuitOpen {
		t.Fatalf("refusals moved the breaker: %+v", st)
	}
	if st.LastErrorClass != "" || st.LastErrorAt != nil {
		t.Fatalf("refusals recorded a provider error: %+v", st)
	}
	if c.TokensUsedToday() != 0 || ts.calls.Load() != 0 {
		t.Fatalf("tokens=%d calls=%d after refusals, want 0/0",
			c.TokensUsedToday(), ts.calls.Load())
	}
	if _, _, err := c.Chat(context.Background(), "s", "small", 100); err != nil {
		t.Fatalf("a small prompt after refusals: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the small prompt sent", ts.calls.Load())
	}
}

func TestPreflight_ThinkingModelCountsTheReasoningReserve(t *testing.T) {
	ts := newToolServer(t, toolReply("ok", nil, 10))
	c := New(&config.LLMConfig{Enabled: true, Endpoint: ts.srv.URL, APIKey: "k",
		Model: "gemini-2.5-flash", TimeoutSeconds: 5, TokenBudgetDaily: 1000000}, noopLog)
	knownLimit(t, c, "gemini-2.5-flash", 20000)
	// 20000 - (100 + 16384) = 3516 input tokens are left.
	if _, _, err := c.Chat(context.Background(), "", promptOf(3516), 100); err != nil {
		t.Fatalf("prompt at the bound refused: %v", err)
	}
	_, _, err := c.Chat(context.Background(), "", promptOf(3517), 100)
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want refusal above the reasoning-adjusted bound", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", ts.calls.Load())
	}
}

func TestPreflight_MaxTokensAtOrAboveLimitRefusesEverything(t *testing.T) {
	err := checkPromptSize(config.LLMConfig{Model: "m"}, 1000, 1, 1000)
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("err = %v: no input room, yet admitted", err)
	}
	if err := checkPromptSize(config.LLMConfig{Model: "m"}, 0, 1, 1000); err != nil {
		t.Fatalf("limit 0 (unknown) guarded: %v", err)
	}
	if err := checkPromptSize(config.LLMConfig{Model: "m"}, -5, 1, 1000); err != nil {
		t.Fatalf("negative limit guarded: %v", err)
	}
}

func TestPreflight_ChatWithToolsHistoryAboveContextBudgetIsSent(t *testing.T) {
	ts := newToolServer(t, toolReply("done", nil, 10))
	c := New(&config.LLMConfig{Enabled: true, Endpoint: ts.srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1000000,
		ContextBudgetTokens: 100}, noopLog)
	knownLimit(t, c, "m", 50000)
	msgs := append(userMsgs(), Message{Role: "user", Content: promptOf(5000)})
	if _, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		ToolOptions{MaxTokens: 256}); err != nil {
		t.Fatalf("history over context_budget_tokens but under the limit: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", ts.calls.Load())
	}
	msgs = append(msgs, Message{Role: "user", Content: promptOf(50000)})
	_, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		ToolOptions{MaxTokens: 256})
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want ErrPromptTooLarge over the model limit", err)
	}
	if ts.calls.Load() != 1 || StatusOf(c).ConsecutiveFailures != 0 {
		t.Fatalf("refused tool call: calls=%d failures=%d", ts.calls.Load(),
			StatusOf(c).ConsecutiveFailures)
	}
}

func TestPreflight_DatabricksAuthIsNeverGuarded(t *testing.T) {
	cfg := config.LLMConfig{Auth: "databricks", Endpoint: "https://w.example", Model: "m"}
	inputLimits.record(modelCacheKey(cfg.Endpoint, ""),
		[]ModelInfo{{ID: "m", InputTokenLimit: 10}})
	if got := inputLimits.limit(cfg); got != 0 {
		t.Fatalf("databricks limit = %d, want 0 (no listing feeds it)", got)
	}
}

func TestInputLimits_ModelsPrefixAndEmptyListing(t *testing.T) {
	cfg := config.LLMConfig{Endpoint: "https://prefix.example/v1", APIKey: "k",
		Model: "models/gemini-x"}
	key := modelCacheKey(cfg.Endpoint, cfg.APIKey)
	inputLimits.record(key, []ModelInfo{{ID: "gemini-x", InputTokenLimit: 777},
		{ID: "no-limit", InputTokenLimit: 0}})
	if got := inputLimits.limit(cfg); got != 777 {
		t.Fatalf("limit for models/ prefix = %d, want 777", got)
	}
	cfg.Model = "no-limit"
	if got := inputLimits.limit(cfg); got != 0 {
		t.Fatalf("a model without a reported limit = %d, want 0", got)
	}
	inputLimits.record(key, nil)
	cfg.Model = "gemini-x"
	if got := inputLimits.limit(cfg); got != 0 {
		t.Fatalf("after a listing without limits = %d, want 0", got)
	}
}

func TestInputLimits_BoundedIdentities(t *testing.T) {
	for i := 0; i < 3*maxInputLimitIdentities; i++ {
		inputLimits.record(fmt.Sprintf("bounded-%d", i),
			[]ModelInfo{{ID: "m", InputTokenLimit: 100}})
	}
	if n := inputLimits.size(); n > maxInputLimitIdentities {
		t.Fatalf("registry holds %d identities, bound %d", n, maxInputLimitIdentities)
	}
}

func TestInputLimits_ConcurrentRecordAndLookup(t *testing.T) {
	cfg := config.LLMConfig{Endpoint: "https://concurrent.example", APIKey: "k",
		Model: "m"}
	key := modelCacheKey(cfg.Endpoint, cfg.APIKey)
	done := make(chan int, 16)
	for g := 0; g < 16; g++ {
		go func(g int) {
			seen := 0
			for i := 0; i < 200; i++ {
				if g%2 == 0 {
					inputLimits.record(key, []ModelInfo{{ID: "m", InputTokenLimit: 4096}})
				} else if l := inputLimits.limit(cfg); l != 0 && l != 4096 {
					seen = l
				}
			}
			done <- seen
		}(g)
	}
	for g := 0; g < 16; g++ {
		if bad := <-done; bad != 0 {
			t.Fatalf("lookup saw a torn limit %d", bad)
		}
	}
	if got := inputLimits.limit(cfg); got != 4096 {
		t.Fatalf("limit after concurrent writes = %d, want 4096", got)
	}
}

func TestContextBudgetBytes(t *testing.T) {
	for _, tc := range []struct{ tokens, want int }{
		{8192, 32768}, {2048, 8192}, {1, 4}, {0, 0}, {-3, 0},
	} {
		cfg := config.LLMConfig{ContextBudgetTokens: tc.tokens}
		if got := ContextBudgetBytes(cfg); got != tc.want {
			t.Errorf("ContextBudgetBytes(%d) = %d, want %d", tc.tokens, got, tc.want)
		}
	}
	var nilClient *Client
	if got := nilClient.ContextBudgetBytes(); got != 0 {
		t.Fatalf("nil client budget = %d, want 0", got)
	}
	c := New(&config.LLMConfig{ContextBudgetTokens: 1000}, noopLog)
	if got := c.ContextBudgetBytes(); got != 4000 {
		t.Fatalf("client budget = %d, want 4000", got)
	}
}

// toolBodyEstimate is the guard's estimate of a ChatWithTools request: the
// JSON wire body, as exchangeTools sends it.
func toolBodyEstimate(t *testing.T, c *Client, msgs []Message, opts ToolOptions) int {
	t.Helper()
	cfg, _ := c.configSnapshot()
	req := buildToolRequest(cfg.Model, msgs, evidenceTools, opts)
	body, err := marshalShaped(req, requestShape(cfg, true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return estimateTokens(string(body))
}

func TestPreflight_ChatWithToolsBoundaryKeepsMaxTokensForTheAnswer(t *testing.T) {
	ts := newToolServer(t, toolReply("done", nil, 10))
	c := toolClient(ts.srv.URL)
	msgs := append(userMsgs(), Message{Role: "user", Content: promptOf(3000)})
	opts := ToolOptions{MaxTokens: 700}
	est := toolBodyEstimate(t, c, msgs, opts)
	knownLimit(t, c, "m", est+700-1)
	_, err := c.ChatWithTools(context.Background(), msgs, evidenceTools, opts)
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want refusal when max_tokens no longer fits", err)
	}
	if ts.calls.Load() != 0 {
		t.Fatalf("calls = %d: a refused tool request reached the provider",
			ts.calls.Load())
	}
	knownLimit(t, c, "m", est+700)
	if _, err := c.ChatWithTools(context.Background(), msgs, evidenceTools,
		opts); err != nil {
		t.Fatalf("tool request at limit - max_tokens refused: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the request at the bound sent", ts.calls.Load())
	}
}
