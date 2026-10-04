// Package ask is Ask Sage (roadmap phase 3): a conversational surface on
// the same bounded tool loop as the investigator (internal/agentloop).
// The model answers a person's (or an agent's) question about one
// monitored database by calling read-only tools over pg_sage's own
// records (findings, actions and their verification outcomes, the trust
// ledger, facts, incidents, investigations, configuration and concepts)
// and the catalog. Every statement of an answer cites the evidence it
// rests on, with its numbers; uncited or ungrounded statements are
// dropped, and the answer says what it could not verify. "Not observed"
// is a valid answer.
//
// Ask Sage can never execute, approve, confirm a fact or bypass the
// policy gate. A caller who may propose can have it open an
// investigation or queue one of pg_sage's own findings for a person's
// approval, through interfaces the wiring supplies; this package imports
// no execution or approval path. Database text, findings, earlier
// answers and model output reach the model as fenced data, never as
// instructions. Its LLM spend comes from its own persisted daily budget
// per database and per user.
package ask

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// Errors, distinguishable with errors.Is; details are wrapped.
var (
	ErrInvalid         = errors.New("ask: invalid request")
	ErrNotFound        = errors.New("ask: not found")
	ErrDisabled        = errors.New("ask: Ask Sage is disabled (ask.enabled)")
	ErrUnavailable     = errors.New("ask: unavailable")
	ErrBudgetExhausted = errors.New("ask: daily budget exhausted")
	// ErrRefused: a write path refused the request (a finding without a
	// typed action or rollback, an invalid investigation subject).
	ErrRefused = errors.New("ask: refused")
	// ErrBlocked: the policy gate blocks the proposal.
	ErrBlocked = errors.New("ask: blocked by policy")
)

// Answer statuses.
const (
	StatusAnswered    = "answered"
	StatusNotObserved = "not_observed"
	StatusBudget      = "budget_exhausted"
	StatusNoModel     = "llm_unavailable"
	StatusIncomplete  = "incomplete"
)

// Action kinds and statuses.
const (
	ActionProposal      = "proposal"
	ActionInvestigation = "investigation"

	ActionQueued  = "queued"
	ActionPending = "already_pending"
	ActionBlocked = "blocked"
	ActionRefused = "refused"
	ActionFailed  = "failed"
	ActionOpened  = "opened"
	ActionJoined  = "joined"
)

// Limits.
const (
	MaxQuestionRunes           = 2000
	MaxMessagesPerConversation = 50
	MaxNotes                   = 5
	MaxNoteRunes               = 300
	MaxClaims                  = 8
	maxTitleRunes              = 80
	historyTurns               = 4
)

// FinalTool is the tool the model answers with.
const FinalTool = "answer"

// Drop reasons of "not observed" notes (claims use agentloop's).
const (
	DropNoteNumbers = "note_with_numbers"
	DropNoteTooLong = "note_too_long"
)

// Caller is who asks. Actor is the stable identity ("user:42",
// "mcp:token:<id>"); MayPropose is the right to open an investigation or
// queue a proposal (operator or admin, or the MCP propose scope); Agent
// marks a program rather than a person. Nobody gets an approve path.
type Caller struct {
	Actor      string
	MayPropose bool
	Agent      bool
}

// Request is one question, optionally continuing a conversation.
type Request struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Question       string `json:"question"`
}

// Statement is one verified sentence of an answer with the evidence ids
// it cites.
type Statement struct {
	Text      string   `json:"text"`
	Citations []string `json:"citations"`
}

// Citation is one piece of evidence an answer cites: its id
// ("finding:42"), kind and reference, the label and digest of exactly
// what the model was shown, and the API path that serves the object.
type Citation struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Label   string `json:"label"`
	Digest  string `json:"digest"`
	APIPath string `json:"api_path,omitempty"`
}

// ActionTaken is an investigation opened or a proposal queued (or the
// reason it was not) during the answer.
type ActionTaken struct {
	Kind          string          `json:"kind"`
	ID            string          `json:"id,omitempty"`
	Status        string          `json:"status"`
	Verdict       string          `json:"verdict,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	SQL           string          `json:"sql,omitempty"`
	RollbackSQL   string          `json:"rollback_sql,omitempty"`
	RollbackClass string          `json:"rollback_class,omitempty"`
	Prediction    json.RawMessage `json:"prediction,omitempty"`
	EvidenceID    string          `json:"evidence_id,omitempty"`
}

// Answer is one answered question as stored and served.
type Answer struct {
	ID             int64                    `json:"id"`
	ConversationID string                   `json:"conversation_id"`
	Database       string                   `json:"database"`
	Question       string                   `json:"question"`
	Status         string                   `json:"status"`
	Text           string                   `json:"text"`
	Statements     []Statement              `json:"statements"`
	NotVerified    []string                 `json:"not_verified"`
	Dropped        []agentloop.DroppedClaim `json:"dropped"`
	Citations      []Citation               `json:"citations"`
	Actions        []ActionTaken            `json:"actions"`
	Stop           string                   `json:"stop"`
	Tokens         int                      `json:"tokens"`
	Transcript     *agentloop.Transcript    `json:"transcript,omitempty"`
	CreatedAt      time.Time                `json:"created_at"`
}

// Conversation is one user's conversation on a database.
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Messages  int       `json:"messages"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Thread is a conversation with its answers, oldest first.
type Thread struct {
	Conversation Conversation `json:"conversation"`
	Answers      []Answer     `json:"answers"`
}
