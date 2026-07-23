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
}

type ProposalRequest struct {
	Scope           Scope
	ExpectedVersion int64
	Profile         string
	Document        json.RawMessage
	Actor           string
	Preview         ImpactPreview
}

type RatifyRequest struct {
	ProposalID      int64
	ExpectedVersion int64
	Actor           string
}
