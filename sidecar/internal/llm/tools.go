package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Tool calling (Sage SRE M0). ChatWithTools is the OpenAI-compatible
// tools/tool_choice exchange used by incident narration. It is strictly
// bounded: no retry ladder (only a wire-shape rejection is re-sent), the client's
// daily and per-database budgets, an optional per-call timeout, and the
// Reconfigure kill switch. Tool execution itself stays with the caller.

// Limits on one ChatWithTools request and reply.
const (
	MaxTools            = 16
	MaxToolCallsPerTurn = 8
)

// Errors returned by ChatWithTools. Callers use errors.Is to degrade to
// deterministic output.
var (
	ErrLLMDisabled        = errors.New("LLM not enabled")
	ErrRateLimited        = errors.New("LLM provider rate limited the request")
	ErrInvalidToolRequest = errors.New("invalid tool request")
	ErrMalformedToolCall  = errors.New("malformed tool call from LLM")
)

// ToolChoice mirrors OpenAI tool_choice string values.
type ToolChoice string

const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceNone     ToolChoice = "none"
	ToolChoiceRequired ToolChoice = "required"
)

// Message is one chat message in a tool-calling conversation.
type Message struct {
	Role       string     // system, user, assistant or tool
	Content    string     // text; may be empty on assistant tool calls
	ToolCalls  []ToolCall // assistant turns that requested tools
	ToolCallID string     // tool results: the call being answered
}

// ToolSpec declares one tool the model may call. Parameters is a JSON
// Schema object; empty means "no arguments".
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is one validated tool invocation requested by the model.
// Arguments is always a JSON object.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolOptions bounds one ChatWithTools call.
type ToolOptions struct {
	MaxTokens  int           // completion cap; 0 uses the client default
	Timeout    time.Duration // 0 uses llm.timeout_seconds only
	ToolChoice ToolChoice    // "" means auto
	// Budget is an optional per-call budget (e.g. an investigation's
	// durable reservation), charged the full serialized prompt plus the
	// completion cap before provider I/O, in addition to the client's
	// daily and per-database budgets.
	Budget Budgeter
	// ReasoningTokens is an explicit reasoning allowance for thinking
	// models: max_tokens is the completion cap plus this allowance instead
	// of the default 16384 reserve. 0 keeps the default; non-thinking
	// models never get a reasoning reserve.
	ReasoningTokens int
}

// ToolResult is the model's reply: final content, tool calls, or both.
type ToolResult struct {
	Content      string
	ToolCalls    []ToolCall
	Tokens       int
	FinishReason string
	// The provider's usage breakdown, as reported (0 when not reported):
	// prompt, completion and reasoning (completion_tokens_details) tokens.
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ChatWithTools sends one tool-calling chat turn. It never retries a
// failure: a rate limit returns ErrRateLimited at once so callers can fall
// back inside their own deadline. Only a request-shape rejection in auto
// mode is re-sent, once per kind (wire_compat.go).
func (c *Client) ChatWithTools(
	ctx context.Context, msgs []Message, tools []ToolSpec, opts ToolOptions,
) (ToolResult, error) {
	if c == nil {
		return ToolResult{}, ErrLLMDisabled
	}
	if err := validateToolRequest(msgs, tools, opts); err != nil {
		return ToolResult{}, err
	}
	requestCtx, cfg, generation, finish := c.beginRequest(ctx)
	defer finish()
	if !configEnabled(cfg) {
		return ToolResult{}, ErrLLMDisabled
	}
	if err := c.checkAvailable(cfg); err != nil {
		return ToolResult{}, err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, opts.Timeout)
		defer cancel()
	}
	req := buildToolRequest(cfg.Model, msgs, tools, opts)
	return c.exchangeTools(ctx, requestCtx, cfg, generation, req, tools, opts.Budget)
}

func validateToolRequest(
	msgs []Message, tools []ToolSpec, opts ToolOptions,
) error {
	if len(msgs) == 0 {
		return fmt.Errorf("%w: no messages", ErrInvalidToolRequest)
	}
	for i, m := range msgs {
		if err := validateMessage(m); err != nil {
			return fmt.Errorf("%w: message %d: %v", ErrInvalidToolRequest, i, err)
		}
	}
	if err := validateToolSpecs(tools); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToolRequest, err)
	}
	switch opts.ToolChoice {
	case "", ToolChoiceAuto, ToolChoiceNone:
	case ToolChoiceRequired:
		if len(tools) == 0 {
			return fmt.Errorf("%w: tool_choice required without tools",
				ErrInvalidToolRequest)
		}
	default:
		return fmt.Errorf("%w: unknown tool_choice %q",
			ErrInvalidToolRequest, opts.ToolChoice)
	}
	if opts.MaxTokens < 0 || opts.Timeout < 0 || opts.ReasoningTokens < 0 {
		return fmt.Errorf("%w: negative max_tokens, timeout or reasoning tokens",
			ErrInvalidToolRequest)
	}
	return nil
}

func validateMessage(m Message) error {
	switch m.Role {
	case "system", "user", "assistant":
		return nil
	case "tool":
		if m.ToolCallID == "" {
			return errors.New("tool message without tool_call_id")
		}
		return nil
	default:
		return fmt.Errorf("unknown role %q", m.Role)
	}
}

func validateToolSpecs(tools []ToolSpec) error {
	if len(tools) > MaxTools {
		return fmt.Errorf("%d tools exceeds the limit of %d", len(tools), MaxTools)
	}
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		if !toolNamePattern.MatchString(t.Name) {
			return fmt.Errorf("invalid tool name %q", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate tool %q", t.Name)
		}
		seen[t.Name] = true
		if len(t.Parameters) > 0 && !isJSONObject(t.Parameters) {
			return fmt.Errorf("tool %q parameters must be a JSON object", t.Name)
		}
	}
	return nil
}

func isJSONObject(raw []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

// normalizeToolCalls validates the model's tool calls against the
// declared tools. Fenced argument JSON is recovered; anything that is not
// a JSON object for a declared tool is ErrMalformedToolCall.
func normalizeToolCalls(raw []wireToolCall, tools []ToolSpec) ([]ToolCall, error) {
	if len(raw) > MaxToolCallsPerTurn {
		return nil, fmt.Errorf("%w: %d calls exceeds the limit of %d",
			ErrMalformedToolCall, len(raw), MaxToolCallsPerTurn)
	}
	declared := make(map[string]bool, len(tools))
	for _, t := range tools {
		declared[t.Name] = true
	}
	out := make([]ToolCall, 0, len(raw))
	for i, w := range raw {
		if !declared[w.Function.Name] {
			return nil, fmt.Errorf("%w: undeclared tool %q",
				ErrMalformedToolCall, w.Function.Name)
		}
		args := strings.TrimSpace(StripJSON(w.Function.Arguments, JSONObject))
		if args == "" {
			args = "{}"
		}
		if !isJSONObject([]byte(args)) {
			return nil, fmt.Errorf("%w: arguments of %s are not a JSON object",
				ErrMalformedToolCall, w.Function.Name)
		}
		id := w.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		out = append(out, ToolCall{ID: id, Name: w.Function.Name,
			Arguments: json.RawMessage(args)})
	}
	return out, nil
}
