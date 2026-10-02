package runbook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/llm"
)

// English -> draft DAG (AI-SRE-SPEC §7.1). The model compiles a playbook
// (e.g. an imported Xata playbook) into a runbook definition through one
// submit_runbook tool call, or JSON content when the provider cannot call
// tools. The reply is decoded strictly and validated against the
// catalogs; a malformed, empty, oversized or invalid reply (or a provider
// error, retried without tools) gets one repair turn that names what was
// wrong. The result is only ever a draft: it runs after a human signs it.

// Compiler limits.
const (
	MaxSourceRunes        = 16000
	DefaultCompileTimeout = 60 * time.Second
	compileToolName       = "submit_runbook"
	compileMaxTokens      = 4000
	maxRepairDetail       = 1500
)

// Rejection reasons.
const (
	RejectMalformed      = "malformed_output"
	RejectEmpty          = "empty_response"
	RejectOversized      = "oversized_output"
	RejectInvalid        = "invalid_definition"
	RejectTimeout        = "timeout"
	RejectRateLimited    = "rate_limited"
	RejectBudget         = "budget_exhausted"
	RejectCooldown       = "cooldown"
	RejectDisabled       = "llm_disabled"
	RejectProvider       = "provider_error"
	RejectInvalidRequest = "invalid_request"
)

var repairable = map[string]bool{RejectMalformed: true, RejectEmpty: true,
	RejectOversized: true, RejectInvalid: true, RejectProvider: true}

// Model is the LLM client surface the compiler uses (*llm.Client).
type Model interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec,
		opts llm.ToolOptions) (llm.ToolResult, error)
}

// CompileRequest is one playbook to compile. Text is untrusted; callers
// redact it first.
type CompileRequest struct {
	Text    string
	Vocab   Vocab
	Timeout time.Duration
}

// Compiled is a validated draft definition.
type Compiled struct {
	Definition Definition
	Turns      int
	// Repaired is the reason the first reply was rejected, "" when the
	// first reply was used.
	Repaired string
}

// Rejection is why the model's draft could not be used.
type Rejection struct {
	Reason   string   `json:"reason"`
	Detail   string   `json:"detail"`
	Problems Problems `json:"problems,omitempty"`
}

func (r *Rejection) Error() string {
	return "runbook draft rejected: " + r.Reason + ": " + r.Detail
}

func rejectf(reason, format string, args ...any) *Rejection {
	return &Rejection{Reason: reason, Detail: truncate(fmt.Sprintf(format, args...), 300)}
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Compile turns req.Text into a validated draft definition. Bad input is
// ErrInvalid (no model call); a parent context that ends returns its
// error; everything else that goes wrong is a *Rejection.
func Compile(ctx context.Context, m Model, req CompileRequest) (Compiled, error) {
	if err := checkSource(req.Text); err != nil {
		return Compiled{}, err
	}
	if m == nil {
		return Compiled{}, rejectf(RejectDisabled, "no model is configured")
	}
	if req.Timeout <= 0 {
		req.Timeout = DefaultCompileTimeout
	}
	d, rej, err := attempt(ctx, m, req, "", true)
	if err != nil || rej == nil {
		return Compiled{Definition: d, Turns: 1}, err
	}
	if !repairable[rej.Reason] {
		return Compiled{}, rej
	}
	d, again, err := attempt(ctx, m, req, repairNote(rej), rej.Reason != RejectProvider)
	switch {
	case err != nil:
		return Compiled{}, err
	case again != nil:
		return Compiled{}, again
	}
	return Compiled{Definition: d, Turns: 2, Repaired: rej.Reason}, nil
}

func checkSource(text string) error {
	switch {
	case strings.TrimSpace(text) == "":
		return fmt.Errorf("%w: the playbook text is empty", ErrInvalid)
	case utf8.RuneCountInString(text) > MaxSourceRunes:
		return fmt.Errorf("%w: the playbook is longer than %d characters", ErrInvalid,
			MaxSourceRunes)
	case strings.IndexFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r'
	}) >= 0:
		return fmt.Errorf("%w: the playbook contains control characters", ErrInvalid)
	}
	return nil
}

func repairNote(rej *Rejection) string {
	note := rej.Reason + ": " + rej.Detail
	if len(rej.Problems) > 0 {
		note = rej.Reason + ": " + rej.Problems.Error()
	}
	return truncate(note, maxRepairDetail)
}

// attempt makes one model call and checks its reply.
func attempt(ctx context.Context, m Model, req CompileRequest, repair string,
	tools bool) (Definition, *Rejection, error) {
	if err := ctx.Err(); err != nil {
		return Definition{}, nil, err
	}
	var specs []llm.ToolSpec
	if tools {
		specs = compileTools()
	}
	res, err := m.ChatWithTools(ctx, compileMessages(req, repair, tools), specs,
		llm.ToolOptions{Timeout: req.Timeout, MaxTokens: compileMaxTokens})
	if err != nil {
		if ctx.Err() != nil {
			return Definition{}, nil, ctx.Err()
		}
		return Definition{}, classify(err), nil
	}
	d, rej := parseReply(res, req.Vocab)
	return d, rej, nil
}

func classify(err error) *Rejection {
	reason := RejectProvider
	var ne net.Error
	switch {
	case errors.Is(err, llm.ErrLLMDisabled):
		reason = RejectDisabled
	case errors.Is(err, llm.ErrRateLimited):
		reason = RejectRateLimited
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		reason = RejectTimeout
	case errors.Is(err, llm.ErrBudgetExhausted):
		reason = RejectBudget
	case errors.Is(err, llm.ErrRequestCooldown):
		reason = RejectCooldown
	case errors.Is(err, llm.ErrEmptyResponse):
		reason = RejectEmpty
	case errors.Is(err, llm.ErrMalformedToolCall):
		reason = RejectMalformed
	case errors.Is(err, llm.ErrInvalidToolRequest):
		reason = RejectInvalidRequest
	}
	return rejectf(reason, "%v", err)
}

// parseReply decodes the tool call (or JSON content) and validates it.
func parseReply(res llm.ToolResult, v Vocab) (Definition, *Rejection) {
	raw, rej := replyText(res)
	if rej != nil {
		return Definition{}, rej
	}
	if len(raw) > MaxDefinitionBytes {
		return Definition{}, rejectf(RejectOversized, "reply is %d bytes, over %d",
			len(raw), MaxDefinitionBytes)
	}
	cleaned := strings.TrimSpace(llm.StripJSON(raw, llm.JSONObject))
	if cleaned == "" {
		return Definition{}, rejectf(RejectEmpty, "the reply is empty")
	}
	d, err := Decode([]byte(cleaned))
	if err != nil {
		return Definition{}, rejectf(RejectMalformed, "not one runbook JSON object: %v", err)
	}
	if problems := Validate(d, v); problems != nil {
		return Definition{}, &Rejection{Reason: RejectInvalid,
			Detail: truncate(problems.Error(), maxRepairDetail), Problems: problems}
	}
	return d, nil
}

func replyText(res llm.ToolResult) (string, *Rejection) {
	switch len(res.ToolCalls) {
	case 0:
		if strings.TrimSpace(res.Content) == "" {
			return "", rejectf(RejectEmpty, "the reply is empty")
		}
		return res.Content, nil
	case 1:
		if res.ToolCalls[0].Name != compileToolName {
			return "", rejectf(RejectMalformed, "called %q, not %s", res.ToolCalls[0].Name,
				compileToolName)
		}
		return string(res.ToolCalls[0].Arguments), nil
	}
	return "", rejectf(RejectMalformed, "%d tool calls, want one %s call",
		len(res.ToolCalls), compileToolName)
}
