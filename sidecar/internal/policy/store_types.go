package policy

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrUnavailable     = errors.New("policy store unavailable")
	ErrNotFound        = errors.New("policy not found")
	ErrVersionConflict = errors.New("policy version conflict")
	ErrInvalidDocument = errors.New("invalid policy document")
	// ErrSecondApprovalRequired: the approval was recorded, and the agent's
	// widening proposal waits for a second person (§6.11, G1-14).
	ErrSecondApprovalRequired = errors.New("a second person must approve this " +
		"widening agent proposal")
	// ErrSponsorCannotApprove: the proposing agent's sponsor may not approve
	// its widening proposal (outside single-operator mode).
	ErrSponsorCannotApprove = errors.New("the proposing agent's sponsor cannot " +
		"approve its widening proposal")
	// ErrReasonRequired: a single-operator approval needs a recorded reason.
	ErrReasonRequired = errors.New("single-operator approval requires a reason")
)

type Scope struct {
	DatabaseID *int64 `json:"database_id,omitempty"`
}

type Policy struct {
	ID        int64           `json:"id"`
	Scope     Scope           `json:"scope"`
	Version   int64           `json:"version"`
	Profile   string          `json:"profile"`
	Document  json.RawMessage `json:"document"`
	UpdatedBy string          `json:"updated_by"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type PendingOutcomeChange struct {
	ActionID int64  `json:"action_id"`
	Before   string `json:"before"`
	After    string `json:"after"`
}

type ImpactPreview struct {
	ChangedPendingOutcomes []PendingOutcomeChange `json:"changed_pending_outcomes"`
}

type Proposal struct {
	ID          int64           `json:"id"`
	Scope       Scope           `json:"scope"`
	BaseVersion int64           `json:"base_version"`
	Profile     string          `json:"profile"`
	Document    json.RawMessage `json:"document"`
	Actor       string          `json:"actor"`
	Preview     ImpactPreview   `json:"preview"`
	CreatedAt   time.Time       `json:"created_at"`
	RatifiedAt  *time.Time      `json:"ratified_at,omitempty"`
	// PrincipalID is the agent that proposed it ("" for a person), and
	// SponsorID that agent's sponsor then (0 = none). Widening reports a
	// proposal that widens its base version (§6.11, G1-14).
	PrincipalID string `json:"principal_id,omitempty"`
	SponsorID   int    `json:"sponsor_id,omitempty"`
	Widening    bool   `json:"widening"`
}

type ProposalRequest struct {
	Scope           Scope
	ExpectedVersion int64
	Profile         string
	Document        json.RawMessage
	Actor           string
	Preview         ImpactPreview
	// Principal is the agent proposing, nil for a person.
	Principal *PrincipalRef
}

type RatifyRequest struct {
	ProposalID      int64
	ExpectedVersion int64
	Actor           string
	// ApproverUserID is the approving person's sage.users id; required
	// for an agent's widening proposal (two people, sponsor excluded).
	ApproverUserID int
	// Reason is recorded with the approval; single-operator mode requires
	// it.
	Reason string
	// SingleOperatorMode is agents.single_operator_mode: one person may
	// approve a widening agent proposal with a reason, and the approval
	// enters the post-hoc review queue.
	SingleOperatorMode bool
}
