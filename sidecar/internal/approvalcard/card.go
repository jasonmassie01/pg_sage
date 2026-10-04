// Package approvalcard builds the approval card of a queued action (roadmap
// 1.5): what pg_sage wants to do and to which objects, the evidence with
// numbers, the model's rationale, the predicted effect, the exact SQL and
// rollback, the blast radius and guardrails, why a person must decide, and
// when the request expires. The same card is served to the UI, rendered
// into Slack and Telegram messages with one-click buttons, and followed up
// with the verification verdict. It decides nothing: approvals go through
// the existing approval path (policy re-authorization, change lease, the
// card's content hash).
package approvalcard

import (
	"errors"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/store"
)

// ErrNotFound means the queue item does not exist.
var ErrNotFound = errors.New("approvalcard: queue item not found")

// Card is one queued action presented for a decision.
type Card struct {
	QueueID        int                `json:"queue_id"`
	Database       string             `json:"database"`
	Title          string             `json:"title"`
	ActionType     string             `json:"action_type"`
	Status         string             `json:"status"`
	Targets        []string           `json:"targets"`
	Finding        *FindingRef        `json:"finding,omitempty"`
	Recommendation *RecommendationRef `json:"recommendation,omitempty"`
	Evidence       []Evidence         `json:"evidence"`
	Rationale      *Rationale         `json:"rationale"`
	Predicted      Predicted          `json:"predicted_effect"`
	SQL            string             `json:"sql"`
	Rollback       Rollback           `json:"rollback"`
	Risk           Risk               `json:"risk"`
	Why            []Reason           `json:"why_approval"`
	ProposedAt     time.Time          `json:"proposed_at"`
	ExpiresAt      time.Time          `json:"expires_at"`
	SnoozedUntil   *time.Time         `json:"snoozed_until,omitempty"`
	SnoozeReason   string             `json:"snooze_reason,omitempty"`
	// Trust is the action class's trust on the database (roadmap 1.2);
	// nil without a trust ledger.
	Trust *Trust `json:"trust,omitempty"`
	// CardHash binds a decision to exactly this content (ContentHash).
	CardHash string `json:"card_hash"`
	// ShadowHistory is the action class's shadow record (roadmap 1.4);
	// not part of the hash: it changes as decisions score, the action not.
	ShadowHistory *shadow.History `json:"shadow_history,omitempty"`
	// VerificationWait is another change still being verified on this
	// change's object (one change per object); approving overrides it. Not
	// part of the hash: it ends when that verdict lands.
	VerificationWait *VerificationWait `json:"verification_wait,omitempty"`
}

// FindingRef is the finding behind the action.
type FindingRef struct {
	ID             int    `json:"id"`
	Category       string `json:"category"`
	Severity       string `json:"severity"`
	Object         string `json:"object"`
	Title          string `json:"title"`
	Recommendation string `json:"recommendation,omitempty"`
}

// RecommendationRef is the immutable recommendation revision approved.
type RecommendationRef struct {
	ID          int64  `json:"id"`
	Revision    int    `json:"revision"`
	ContentHash string `json:"content_hash"`
}

// Evidence is one cited fact: a finding, a metric with its number, a
// query, a plan or the policy decision; Ref names where it comes from.
type Evidence struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Value string `json:"value"`
	Ref   string `json:"ref"`
}

// Rationale is why the producer proposes the change: "llm" when a model
// wrote it, "rule" for a deterministic recommendation.
type Rationale struct {
	Source     string   `json:"source"`
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// Predicted is the effect pg_sage expects.
type Predicted struct {
	ImprovementPct     *float64 `json:"improvement_pct,omitempty"`
	EstimatedSizeBytes *int64   `json:"estimated_size_bytes,omitempty"`
	AffectedQueries    []string `json:"affected_queries,omitempty"`
	QueryIDs           []int64  `json:"query_ids,omitempty"`
	WhatIfVerdict      string   `json:"what_if_verdict,omitempty"`
	WhatIfReason       string   `json:"what_if_reason,omitempty"`
	// Method is how the effect was predicted: "hypopg" (what-if verified)
	// or "llm_estimate".
	Method string `json:"method,omitempty"`
	// Forecast is the richer, metric-level prediction (baseline, predicted
	// value, interval) recorded with every action by the predicted-effect
	// work (roadmap 1.3); nil until that producer fills it.
	Forecast *Forecast `json:"forecast,omitempty"`
}

// Forecast is a predicted change of one metric.
type Forecast struct {
	Metric     string   `json:"metric"`
	Unit       string   `json:"unit"`
	Method     string   `json:"method"`
	Baseline   float64  `json:"baseline"`
	Predicted  float64  `json:"predicted"`
	Low        *float64 `json:"low,omitempty"`
	High       *float64 `json:"high,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// Rollback is how the change is undone.
type Rollback struct {
	Class string `json:"class"`
	SQL   string `json:"sql"`
	Note  string `json:"note,omitempty"`
}

// Risk is the risk tier, blast radius, lock taken and guardrails.
type Risk struct {
	Tier        string   `json:"tier"`
	BlastRadius string   `json:"blast_radius"`
	Lock        string   `json:"lock"`
	Guardrails  []string `json:"guardrails"`
	PostChecks  []string `json:"post_checks"`
}

// Reason is one reason the action needs a person; Code is stable.
type Reason struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Inputs is everything a card is assembled from.
type Inputs struct {
	Database   string
	Action     store.QueuedAction
	Finding    *FindingRow
	Revision   *RevisionRow
	Decision   *DecisionRow
	Rejection  *RejectionRow
	Snooze     *SnoozeRow
	Contract   *executor.ActionContract
	TrustLevel string
	// Trust is the ledger row of the action's class (nil: no ledger, or
	// a class the ledger does not judge); TrustErr an unreadable ledger.
	Trust    *earned.TrustRow
	TrustErr error
	// Waits are the changes in flight on the action's objects; WaitsErr an
	// unreadable in-flight state.
	Waits    []policy.PendingVerification
	WaitsErr error
	Now      time.Time
}

// FindingRow is the finding of a queued action.
type FindingRow struct {
	ID             int
	Category       string
	Severity       string
	ObjectType     string
	Object         string
	Title          string
	Recommendation string
	Detail         map[string]any
}

// RevisionRow is the recommendation revision a queued action is pinned to.
type RevisionRow struct {
	ID             int64
	Revision       int
	ContentHash    string
	Title          string
	Recommendation string
	Evidence       map[string]any
}

// DecisionRow is the policy-gate decision that queued the action.
type DecisionRow struct {
	ID         int64
	Verdict    string
	RiskTier   string
	Reason     string
	Guardrails []string
	EvidenceID string
	CreatedAt  time.Time
}

// RejectionRow is an earlier operator rejection of the same SQL.
type RejectionRow struct {
	QueueID   int
	DecidedAt time.Time
	Reason    string
}

// SnoozeRow is the operator's snooze of the item.
type SnoozeRow struct {
	Until  time.Time
	By     int
	Reason string
}

func itoa(i int) string { return strconv.Itoa(i) }
