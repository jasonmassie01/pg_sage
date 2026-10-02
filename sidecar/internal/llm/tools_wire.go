package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// OpenAI-compatible wire types for tool calling.

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type toolChatRequest struct {
	Model      string        `json:"model"`
	Messages   []wireMessage `json:"messages"`
	Tools      []wireTool    `json:"tools,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
	MaxTokens  int           `json:"max_tokens,omitempty"`
	// Set by shaped (wire_compat.go) for models that need them.
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
}

// startShaped is a request's starting wire shape and its JSON body.
func startShaped(
	cfg config.LLMConfig, req toolChatRequest, tools bool,
) (wireShape, []byte, error) {
	shape := requestShape(cfg, tools)
	body, err := marshalShaped(req, shape)
	return shape, body, err
}

// marshalShaped is the JSON body of the request in the given wire shape.
func marshalShaped(req toolChatRequest, shape wireShape) ([]byte, error) {
	body, err := json.Marshal(req.shaped(shape))
	if err != nil {
		return nil, fmt.Errorf("marshal tool request: %w", err)
	}
	return body, nil
}

// shaped returns the request in the given wire shape; the receiver keeps
// MaxTokens as the cap for budgeting.
func (r toolChatRequest) shaped(shape wireShape) toolChatRequest {
	if shape.completionTokens {
		r.MaxTokens, r.MaxCompletionTokens = 0, r.MaxTokens
	}
	r.ReasoningEffort = shape.effort
	return r
}

type toolChatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		Details          struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

var emptyToolParameters = json.RawMessage(`{"type":"object","properties":{}}`)

func buildToolRequest(
	model string, msgs []Message, tools []ToolSpec, opts ToolOptions,
) toolChatRequest {
	req := toolChatRequest{
		Model:     model,
		Messages:  make([]wireMessage, 0, len(msgs)),
		MaxTokens: toolMaxTokens(model, opts),
	}
	for _, m := range msgs {
		wm := wireMessage{Role: m.Role, Content: m.Content,
			ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: tc.ID,
				Type: "function", Function: wireFunction{Name: tc.Name,
					Arguments: string(tc.Arguments)}})
		}
		req.Messages = append(req.Messages, wm)
	}
	for _, t := range tools {
		params := t.Parameters
		if len(params) == 0 {
			params = emptyToolParameters
		}
		req.Tools = append(req.Tools, wireTool{Type: "function",
			Function: wireToolFunction{Name: t.Name,
				Description: t.Description, Parameters: params}})
	}
	if len(tools) > 0 {
		req.ToolChoice = string(opts.ToolChoice)
		if req.ToolChoice == "" {
			req.ToolChoice = string(ToolChoiceAuto)
		}
	}
	return req
}

// exchangeTools performs one budgeted, throttled provider exchange.
func (c *Client) exchangeTools(
	ctx, requestCtx context.Context, cfg config.LLMConfig,
	generation uint64, req toolChatRequest, tools []ToolSpec, call Budgeter,
) (ToolResult, error) {
	shape, body, err := startShaped(cfg, req, len(tools) > 0)
	if err != nil {
		return ToolResult{}, err
	}
	key := requestThrottleKey(cfg, "tools", string(body))
	if err := c.acquireThrottle(key, cfg.CooldownSeconds); err != nil {
		return ToolResult{}, err
	}
	success := false
	defer func() { c.releaseThrottle(key, success) }()
	callHeld, err := reserveCall(call, estimateTokens(string(body))+req.MaxTokens)
	if err != nil {
		return ToolResult{}, err
	}
	reservation, err := c.reserveBudget(cfg, req.MaxTokens,
		externalReservation(string(body), "", req.MaxTokens))
	if err != nil {
		settleCall(call, callHeld, 0)
		return ToolResult{}, err
	}
	reconciled := false
	defer func() {
		if !reconciled {
			c.releaseBudget(reservation)
			settleCall(call, callHeld, 0)
		}
	}()
	resp, err := c.postToolsAdapting(ctx, requestCtx, cfg, req, shape, len(tools) > 0)
	if err != nil {
		return ToolResult{}, err
	}
	if !c.generationCurrent(generation) {
		return ToolResult{}, fmt.Errorf("%w: reconfigured during request",
			ErrLLMDisabled)
	}
	c.recordSuccess()
	res := toolResult(resp, string(body))
	c.reconcileBudget(reservation, res.Tokens)
	settleCall(call, callHeld, res.Tokens)
	reconciled = true
	if err := finishToolResult(&res, resp, tools); err != nil {
		return res, err
	}
	success = true
	return res, nil
}

func toolResult(resp *toolChatResponse, body string) ToolResult {
	choice := resp.Choices[0]
	tokens := resp.Usage.TotalTokens
	if tokens <= 0 {
		tokens = estimateTokens(body, choice.Message.Content)
	}
	return ToolResult{Content: choice.Message.Content, Tokens: tokens,
		FinishReason: choice.FinishReason, PromptTokens: resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		ReasoningTokens:  resp.Usage.Details.ReasoningTokens}
}

// toolMaxTokens is the completion cap of a tool call: the default
// normalization, or for a thinking model with an explicit reasoning
// allowance, the cap plus that allowance instead of the default reserve.
func toolMaxTokens(model string, opts ToolOptions) int {
	if opts.ReasoningTokens <= 0 || !isThinkingModel(model) {
		return normalizedMaxTokens(model, opts.MaxTokens)
	}
	return normalizedMaxTokens("", opts.MaxTokens) + opts.ReasoningTokens
}

func finishToolResult(
	res *ToolResult, resp *toolChatResponse, tools []ToolSpec,
) error {
	calls, err := normalizeToolCalls(resp.Choices[0].Message.ToolCalls, tools)
	if err != nil {
		return err
	}
	res.ToolCalls = calls
	if strings.TrimSpace(res.Content) == "" && len(calls) == 0 {
		return fmt.Errorf("%w (finish_reason=%s)", ErrEmptyResponse,
			res.FinishReason)
	}
	return nil
}

// postToolsAdapting sends the request, re-sending it only when the
// provider asks for another wire shape (wire_compat.go).
func (c *Client) postToolsAdapting(
	ctx, requestCtx context.Context, cfg config.LLMConfig, req toolChatRequest,
	shape wireShape, tools bool,
) (*toolChatResponse, error) {
	var out *toolChatResponse
	err := c.withAdaptation(cfg, shape, func(s wireShape) error {
		body, err := marshalShaped(req, s)
		if err != nil {
			return err
		}
		resp, err := c.postTools(ctx, requestCtx, cfg, body, s, tools)
		out = resp
		return err
	})
	return out, err
}

// postTools sends the request once. A 429 is ErrRateLimited; other
// provider failures feed the circuit breaker as in Chat.
func (c *Client) postTools(
	ctx, requestCtx context.Context, cfg config.LLMConfig, body []byte,
	shape wireShape, tools bool,
) (*toolChatResponse, error) {
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		chatEndpoint(cfg.Endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tool request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if shouldRecordProviderFailure(ctx, requestCtx) {
			c.recordFailure()
		}
		return nil, providerRequestError("LLM tool request", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return c.decodeToolResponse(resp, cfg, shape, tools)
}

func (c *Client) decodeToolResponse(
	resp *http.Response, cfg config.LLMConfig, shape wireShape, tools bool,
) (*toolChatResponse, error) {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.recordFailure()
		return nil, fmt.Errorf("read tool response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		c.recordFailure()
		return nil, fmt.Errorf("%w (status 429)", ErrRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(cfg, shape, tools, resp.StatusCode, respBody,
			fmt.Errorf("LLM API error %d: %s", resp.StatusCode,
				redactProviderText(string(respBody))))
	}
	var out toolChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		c.recordFailure()
		return nil, fmt.Errorf("unmarshal tool response: %w", err)
	}
	if len(out.Choices) == 0 {
		c.recordFailure()
		return nil, fmt.Errorf("no choices in tool response")
	}
	return &out, nil
}
