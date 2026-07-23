package mcp

import (
	"context"
	"encoding/json"
)

type PolicyRequest struct {
	DatabaseID *int64 `json:"database_id,omitempty"`
}
type PolicyResult struct {
	DatabaseID *int64 `json:"database_id,omitempty"`
	Version    int64  `json:"version"`
	Profile    string `json:"profile"`
}
type PolicyProposalRequest struct {
	DatabaseID   *int64          `json:"database_id,omitempty"`
	Delta        json.RawMessage `json:"delta"`
	CallerClaims map[string]any  `json:"caller_claims,omitempty"`
}
type DryRunImpact struct {
	NewlyAllowed []string `json:"newly_allowed"`
	NewlyBlocked []string `json:"newly_blocked"`
}
type PolicyProposalResult struct {
	ProposalID   int64        `json:"proposal_id"`
	DryRunImpact DryRunImpact `json:"dry_run_impact"`
}
type ChangeRequest struct {
	DatabaseID   *int64          `json:"database_id,omitempty"`
	Intent       json.RawMessage `json:"intent"`
	CallerClaims map[string]any  `json:"caller_claims,omitempty"`
}
type ChangeResult struct {
	Decision   string `json:"decision"`
	EvidenceID string `json:"evidence_id"`
	Reason     string `json:"reason"`
	Outcome    any    `json:"outcome,omitempty"`
}
type LedgerRequest struct {
	Filter json.RawMessage `json:"filter"`
}
type LedgerEntry struct {
	EvidenceID string `json:"evidence_id"`
	Decision   string `json:"decision"`
	Feature    string `json:"feature"`
}
type LedgerResult struct {
	Entries []LedgerEntry `json:"entries"`
}

type Backend interface {
	GetPolicy(context.Context, PolicyRequest) (PolicyResult, error)
	ProposePolicyChange(context.Context, PolicyProposalRequest) (PolicyProposalResult, error)
	RequestChange(context.Context, ChangeRequest) (ChangeResult, error)
	GetLedger(context.Context, LedgerRequest) (LedgerResult, error)
}

// IntentBackend handles the prescribed agent-native tools. The narrower
// legacy Backend remains supported for protocol compatibility.
type IntentBackend interface {
	RequestIntent(context.Context, string, json.RawMessage) (any, error)
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
