// Package agentloop is a bounded, read-only tool-calling loop (roadmap
// 2.1). A model plans calls over a closed set of tools the caller
// supplies; every call is recorded in a transcript, every citable result
// becomes evidence with an alias (E1, E2, ...) and a digest, and the
// final answer's claims survive only when they cite evidence the run
// holds. The loop is bounded by model steps, tool calls, a tool cost
// budget (probes), wall clock and tokens, whatever the model does.
//
// It speaks OpenAI-compatible tool calling through the LLM client and
// falls back to a JSON action protocol (one {"tool", "args"} object per
// reply, fences and prose tolerated) when the provider refuses tools.
// Nothing here can mutate anything: tools are the caller's read-only
// functions and the loop executes nothing else. The Sage SRE
// investigator is its first user; Ask Sage reuses it.
package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Model is the chat surface the loop drives; *llm.Client implements it.
type Model interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec,
		opts llm.ToolOptions) (llm.ToolResult, error)
}

// Protocol is how the model asks for tools.
type Protocol string

// Protocols.
const (
	// ProtocolAuto starts with native tool calls and switches to JSON
	// actions once if the provider refuses them before any native reply.
	ProtocolAuto   Protocol = "auto"
	ProtocolNative Protocol = "native"
	ProtocolJSON   Protocol = "json"
)

// Tool is one read-only capability offered to the model. Cost is
// charged against Budget.MaxCost for every call that runs (probes cost
// 1; derived views are free). Run returns ErrInvalidArgs (wrapped) for
// arguments it refuses before doing anything, or Abort(err) to end the
// whole run.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Cost        int
	Run         func(ctx context.Context, args json.RawMessage) (Output, error)
}

// Output is one tool result. Text is shown to the model as untrusted
// data; Evidence is set when the result is citable.
type Output struct {
	Status   string
	Text     string
	Evidence *Evidence
}

// Evidence is a citable result: ID is its durable id, Digest the hash of
// the stored result, Label a short "what and status" line, Text the
// grounding text claims are checked against.
type Evidence struct {
	ID     string
	Digest string
	Label  string
	Text   string
}

// Errors.
var (
	// ErrInvalidArgs: a tool refused its arguments; nothing ran.
	ErrInvalidArgs = errors.New("invalid tool arguments")
	// ErrInvalidConfig: the run cannot start.
	ErrInvalidConfig = errors.New("invalid agent loop configuration")
)

// AbortError ends a run from a tool or the model (a lost lease, an
// unavailable store); Run returns its cause.
type AbortError struct{ Err error }

func (e *AbortError) Error() string { return "agent loop aborted: " + e.Err.Error() }
func (e *AbortError) Unwrap() error { return e.Err }

// Abort wraps err so that the loop stops and returns it.
func Abort(err error) error {
	if err == nil {
		return nil
	}
	return &AbortError{Err: err}
}

// Budget bounds one run. MaxSteps counts model calls (the last one may
// only conclude); MaxCalls counts tool calls asked for; MaxCost sums the
// cost of tools that ran; MaxTokens counts reported (else estimated)
// tokens; StepTokens is each call's completion cap and StepTimeout its
// time cap within Wall.
type Budget struct {
	MaxSteps    int
	MaxCalls    int
	MaxCost     int
	Wall        time.Duration
	MaxTokens   int
	StepTokens  int
	StepTimeout time.Duration
}

// Final is the tool the model concludes with. Its arguments must be a
// JSON object; a "claims" array in it is filtered by citation.
type Final struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Config is one run.
type Config struct {
	System   string
	Task     string
	Tools    []Tool
	Final    Final
	Seed     []Evidence // citable before any call: E1..En
	Budget   Budget
	Protocol Protocol
	// Ground is an extra claim check (numbers grounded in the cited
	// evidence); nil accepts every cited claim.
	Ground    func(text string, cited []Evidence) error
	MaxClaims int              // 0: DefaultMaxClaims
	Now       func() time.Time // nil: time.Now
}

// Claim is one statement of the final answer: aliases in, durable ids
// out.
type Claim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// DroppedClaim is a claim the citation filter removed, and why.
type DroppedClaim struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// Limits.
const (
	MaxBadReplies    = 3
	MaxClaimRunes    = 1200
	DefaultMaxClaims = 5
	maxPlanRunes     = 500
	maxResultRunes   = 4000
	maxNoteRunes     = 300
	maxArgsBytes     = 512
)

// Stop reasons.
const (
	StopFinal       = "final"
	StopMaxSteps    = "max_steps"
	StopWall        = "wall_clock"
	StopTokens      = "token_budget"
	StopRateLimited = "rate_limited"
	StopTimeout     = "timeout"
	StopBudget      = "budget_exhausted"
	StopDisabled    = "llm_disabled"
	StopCooldown    = "cooldown"
	StopProvider    = "provider_error"
	StopMalformed   = "malformed_replies"
)

// Rejection reasons, counted in Transcript.Rejected.
const (
	RejectForbiddenTool  = "forbidden_tool"
	RejectInvalidArgs    = "invalid_args"
	RejectCallBudget     = "call_budget"
	RejectCostBudget     = "cost_budget"
	RejectDuplicate      = "duplicate_call"
	RejectMalformedReply = "malformed_reply"
	RejectEmptyReply     = "empty_reply"
	RejectBesideFinal    = "beside_final"
)

// Citation filter drop reasons.
const (
	DropUncited         = "uncited"
	DropUnknownEvidence = "unknown_evidence"
	DropUngrounded      = "ungrounded"
	DropEmpty           = "empty"
	DropTooLong         = "too_long"
	DropDuplicate       = "duplicate"
	DropOverLimit       = "over_limit"
	DropMalformed       = "malformed"
)

// Step statuses besides a tool's own.
const (
	StatusRejected  = "rejected"
	StatusToolError = "tool_error"
	StatusFinal     = "final"
	StatusProtocol  = "protocol"
)

// Step is one recorded event: a tool call (run or refused), a refused
// reply, the protocol switch or the final answer.
type Step struct {
	Seq        int             `json:"seq"`
	ModelCall  int             `json:"model_call"`
	Tool       string          `json:"tool,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Status     string          `json:"status"`
	Alias      string          `json:"alias,omitempty"`
	EvidenceID string          `json:"evidence_id,omitempty"`
	Digest     string          `json:"digest,omitempty"`
	Cost       int             `json:"cost,omitempty"`
	ElapsedMS  int64           `json:"elapsed_ms"`
	Note       string          `json:"note,omitempty"`
}

// Transcript is everything a run did.
type Transcript struct {
	Protocol   Protocol       `json:"protocol"`
	Plan       string         `json:"plan,omitempty"`
	Steps      []Step         `json:"steps"`
	ModelCalls int            `json:"model_calls"`
	ToolCalls  int            `json:"tool_calls"`
	Cost       int            `json:"cost"`
	Tokens     int            `json:"tokens"`
	Rejected   map[string]int `json:"rejected,omitempty"`
	Stop       string         `json:"stop"`
	StopDetail string         `json:"stop_detail,omitempty"`
}

// Result is a finished run. Final is nil unless the model concluded.
type Result struct {
	Final      json.RawMessage
	Claims     []Claim
	Dropped    []DroppedClaim
	Transcript Transcript
}
