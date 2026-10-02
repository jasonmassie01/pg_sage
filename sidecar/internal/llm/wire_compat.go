package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/config"
)

// OpenAI compatibility. Current OpenAI models (gpt-5*, gpt-6*, o-series)
// reject max_tokens and ask for max_completion_tokens, and reject function
// tools in /v1/chat/completions unless reasoning_effort is "none". Other
// OpenAI-compatible providers (Gemini, Groq, Ollama, ...) accept the
// classic shape. In auto mode (llm.token_parameter, llm.tool_reasoning_
// effort) every request starts in the classic shape; a 400 that asks for
// one of these changes is re-sent once with it, and the change is
// remembered per endpoint and model for the whole process.

// wireShape is how one request names its completion cap and which
// reasoning_effort it sends with tools.
type wireShape struct {
	completionTokens bool   // max_completion_tokens instead of max_tokens
	effort           string // reasoning_effort of a tool request; "" = unset
}

type adaptation int

const (
	adaptNone adaptation = iota
	adaptCompletionTokens
	adaptToolEffortNone
)

// adaptError is a provider rejection the request can be re-sent for. It
// is not a provider failure: the circuit breaker never sees it.
type adaptError struct {
	kind adaptation
	err  error
}

func (e *adaptError) Error() string { return e.err.Error() }
func (e *adaptError) Unwrap() error { return e.err }

// learnedShapes is process-wide: fleet mode builds one client per database
// and purpose, all talking to the same few models.
var learnedShapes = struct {
	sync.Mutex
	m map[string]wireShape
}{m: make(map[string]wireShape)}

func shapeKey(cfg config.LLMConfig) string {
	return chatEndpoint(cfg.Endpoint) + "\x00" + cfg.Model
}

func autoTokens(cfg config.LLMConfig) bool {
	return cfg.TokenParameter == "" || cfg.TokenParameter == config.LLMTokenParameterAuto
}

func autoEffort(cfg config.LLMConfig) bool {
	return cfg.ToolReasoningEffort == "" ||
		cfg.ToolReasoningEffort == config.LLMToolReasoningEffortAuto
}

// requestShape is the starting shape of a request: explicit settings win;
// auto starts from what was learned for this endpoint and model.
// reasoning_effort is only ever sent with tools.
func requestShape(cfg config.LLMConfig, tools bool) wireShape {
	learnedShapes.Lock()
	learned := learnedShapes.m[shapeKey(cfg)]
	learnedShapes.Unlock()
	shape := wireShape{completionTokens: learned.completionTokens}
	if !autoTokens(cfg) {
		shape.completionTokens =
			cfg.TokenParameter == config.LLMTokenParameterMaxCompletionTokens
	}
	switch {
	case !tools, cfg.ToolReasoningEffort == config.LLMToolReasoningEffortOmit:
	case autoEffort(cfg):
		shape.effort = learned.effort
	default:
		shape.effort = cfg.ToolReasoningEffort
	}
	return shape
}

// with returns the shape with one adaptation applied.
func (s wireShape) with(kind adaptation) wireShape {
	switch kind {
	case adaptCompletionTokens:
		s.completionTokens = true
	case adaptToolEffortNone:
		s.effort = config.LLMToolReasoningEffortNone
	}
	return s
}

// adaptationFor maps a provider rejection to the adaptation it asks for.
// Only auto settings adapt, and only to a shape this request does not
// already have, so each request is re-sent at most once per kind.
func adaptationFor(
	cfg config.LLMConfig, shape wireShape, tools bool, status int, body []byte,
) adaptation {
	if status != http.StatusBadRequest {
		return adaptNone
	}
	msg, param, code := providerError(body)
	if autoTokens(cfg) && !shape.completionTokens &&
		asksForCompletionTokens(msg, param, code) {
		return adaptCompletionTokens
	}
	if tools && autoEffort(cfg) && shape.effort != config.LLMToolReasoningEffortNone &&
		strings.Contains(msg, "reasoning_effort") && strings.Contains(msg, "none") {
		return adaptToolEffortNone
	}
	return adaptNone
}

func asksForCompletionTokens(msg, param, code string) bool {
	if strings.Contains(msg, "max_completion_tokens") && strings.Contains(msg, "max_tokens") {
		return true
	}
	return param == "max_tokens" && code == "unsupported_parameter"
}

// providerError returns the lower-cased error message, param and code of
// an OpenAI-style error body; a body that is not one is its own message.
func providerError(body []byte) (string, string, string) {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	msg := string(body)
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	param, _ := parsed.Error.Param.(string)
	code, _ := parsed.Error.Code.(string)
	return strings.ToLower(msg), strings.ToLower(param), strings.ToLower(code)
}

// learn records an adaptation for the endpoint and model and reports
// whether it is new, so it is logged once per process.
func learn(cfg config.LLMConfig, kind adaptation) bool {
	key := shapeKey(cfg)
	learnedShapes.Lock()
	defer learnedShapes.Unlock()
	before := learnedShapes.m[key]
	after := before.with(kind)
	learnedShapes.m[key] = after
	return after != before
}

// withAdaptation sends a request, re-sending it once per adaptation the
// provider asks for. It terminates: every retry adds an adaptation the
// shape did not have, and adaptationFor never repeats one.
func (c *Client) withAdaptation(
	cfg config.LLMConfig, shape wireShape, send func(wireShape) error,
) error {
	for {
		err := send(shape)
		var ae *adaptError
		if !errors.As(err, &ae) {
			return err
		}
		if learn(cfg, ae.kind) {
			c.logAdaptation(cfg.Model, ae.kind)
		}
		shape = shape.with(ae.kind)
	}
}

func (c *Client) logAdaptation(model string, kind adaptation) {
	if kind == adaptCompletionTokens {
		c.logFn("llm", "model %s rejects max_tokens; sending "+
			"max_completion_tokens from now on (llm.token_parameter=auto)", model)
		return
	}
	c.logFn("llm", "model %s rejects function tools without reasoning_effort; "+
		"sending reasoning_effort \"none\" with tools from now on "+
		"(llm.tool_reasoning_effort=auto)", model)
}

// statusError builds the provider error of a non-200 reply: an adaptError
// when the request can be re-sent in another shape, otherwise a provider
// failure recorded by the circuit breaker.
func (c *Client) statusError(
	cfg config.LLMConfig, shape wireShape, tools bool, status int, body []byte,
	apiErr error,
) error {
	if kind := adaptationFor(cfg, shape, tools, status, body); kind != adaptNone {
		return &adaptError{kind: kind, err: apiErr}
	}
	c.recordFailure()
	return apiErr
}
