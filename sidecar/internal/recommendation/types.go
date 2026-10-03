// Package recommendation is the durable recommendation state machine
// (MASTER-SPEC §5.1 #5, Codex C04/C05/C07/C15). A recommendation is a
// head row whose identity is database + category + canonical target +
// action type (+ index fingerprint), with immutable revisions that pin
// the forward SQL, inverse SQL, evidence, preconditions and policy
// version under a content hash. Every state change is a compare-and-set
// on (id, state, revision) and is recorded in an append-only history.
package recommendation

import (
	"errors"
	"time"
)

var (
	// ErrConflict reports a lost compare-and-set: the row's state or
	// revision changed since it was read.
	ErrConflict = errors.New("recommendation changed concurrently")
	// ErrIllegalTransition reports a transition the state machine forbids.
	ErrIllegalTransition = errors.New("illegal recommendation transition")
	// ErrRevised reports an approval that no longer matches the current
	// revision: the content changed and needs a new approval (C04).
	ErrRevised = errors.New("recommendation revised since approval; re-approval required")
	// ErrNotFound reports a missing recommendation.
	ErrNotFound = errors.New("recommendation not found")
)

// Proposal is one analyzer finding offered as a recommendation.
type Proposal struct {
	DatabaseName   string
	Category       string
	Target         string
	ObjectType     string
	Title          string
	Severity       string
	ActionRisk     string
	Recommendation string
	ForwardSQL     string
	InverseSQL     string
	Evidence       map[string]any
	PolicyVersion  *int64
}

// Outcome says what Propose did.
type Outcome string

const (
	// OutcomeCreated inserted a new head at revision 1.
	OutcomeCreated Outcome = "created"
	// OutcomeRevised appended a revision with new content.
	OutcomeRevised Outcome = "revised"
	// OutcomeUnchanged saw the current content again.
	OutcomeUnchanged Outcome = "unchanged"
	// OutcomeHeld left an in-flight or refused recommendation untouched.
	OutcomeHeld Outcome = "held"
	// OutcomeNoFinding created nothing: no open finding backs the proposal
	// (it was just resolved, or suppressed).
	OutcomeNoFinding Outcome = "no_open_finding"
)

// ProposeResult is the head after Propose and what happened to it.
type ProposeResult struct {
	Recommendation Recommendation
	Outcome        Outcome
}

// Recommendation is the head row.
type Recommendation struct {
	ID               int64      `json:"id"`
	IdentityKey      string     `json:"identity_key"`
	DatabaseName     string     `json:"database_name"`
	Category         string     `json:"category"`
	Target           string     `json:"target"`
	ActionType       string     `json:"action_type"`
	IndexFingerprint string     `json:"index_fingerprint"`
	FindingID        *int64     `json:"finding_id"`
	State            State      `json:"state"`
	Revision         int        `json:"revision"`
	ContentHash      string     `json:"content_hash"`
	ApprovedRevision *int       `json:"approved_revision"`
	ApprovedHash     string     `json:"approved_hash"`
	ApprovedBy       string     `json:"approved_by"`
	ApprovedAt       *time.Time `json:"approved_at"`
	AttemptCount     int        `json:"attempt_count"`
	RetryBudget      int        `json:"retry_budget"`
	NextAttemptAt    *time.Time `json:"next_attempt_at"`
	LeaseUntil       *time.Time `json:"lease_until"`
	ActionLogID      *int64     `json:"action_log_id"`
	Verdict          string     `json:"verdict"`
	Reason           string     `json:"reason"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LastSeenAt       time.Time  `json:"last_seen_at"`
	// due is whether next_attempt_at has passed, by the database clock.
	due bool
}

// Revision is one immutable version of a recommendation's content.
type Revision struct {
	RecommendationID int64          `json:"recommendation_id"`
	Revision         int            `json:"revision"`
	ContentHash      string         `json:"content_hash"`
	ForwardSQL       string         `json:"forward_sql"`
	InverseSQL       string         `json:"inverse_sql"`
	Evidence         map[string]any `json:"evidence"`
	Preconditions    map[string]any `json:"preconditions"`
	PolicyVersion    *int64         `json:"policy_version"`
	Title            string         `json:"title"`
	Severity         string         `json:"severity"`
	ObjectType       string         `json:"object_type"`
	ActionRisk       string         `json:"action_risk"`
	Recommendation   string         `json:"recommendation"`
	Source           string         `json:"source"`
	CreatedAt        time.Time      `json:"created_at"`
}

// Transition is one recorded state change. From is empty on creation.
type Transition struct {
	ID               int64     `json:"id"`
	RecommendationID int64     `json:"recommendation_id"`
	From             State     `json:"from_state"`
	To               State     `json:"to_state"`
	Revision         int       `json:"revision"`
	Actor            string    `json:"actor"`
	Reason           string    `json:"reason"`
	ActionLogID      *int64    `json:"action_log_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// Candidate is an actionable head with its current revision.
type Candidate struct {
	Recommendation
	Current Revision
}

// ClaimRequest moves a recommendation into applying. ApproveAs approves a
// proposed recommendation first (policy or operator actor); empty means
// the recommendation must already be approved (or failed and due).
type ClaimRequest struct {
	ID        int64
	Revision  int
	ApproveAs string
	Reason    string
	Lease     time.Duration
}

// Claim is the durable ownership of one apply attempt.
type Claim struct {
	ID          int64
	Revision    int
	ContentHash string
	Attempt     int
}

// Freshness is the re-check RunCycle makes before acting on a candidate.
type Freshness struct {
	Fresh     bool
	Supersede bool
	Reason    string
}

// ListFilter selects heads for the read API.
type ListFilter struct {
	DatabaseName string
	State        State
	Limit        int
}

// MigrationReport counts what MigrateLegacy mapped.
type MigrationReport struct {
	Findings          int
	QueueLinked       int
	ApprovalsMigrated int
	Skipped           int
	// InversesRepaired counts open findings and unapproved revisions
	// whose CREATE INDEX rollback dropped another index.
	InversesRepaired int
}

const (
	// SourceAnalyzer marks revisions written by the analyzer.
	SourceAnalyzer = "analyzer"
	// SourceMigrated marks revisions created from pre-existing findings
	// or approvals when the state machine was installed.
	SourceMigrated = "migrated"
	// ActorPolicy prefixes approvals granted by the standing policy gate.
	ActorPolicy = "policy"
	// ActorAnalyzer records analyzer-driven transitions.
	ActorAnalyzer = "analyzer"
	// ActorExecutor records executor-driven transitions.
	ActorExecutor = "executor"
	// DefaultRetryBudget is how many retries follow a first failed apply
	// (three attempts in all, matching the executor's failure cap).
	DefaultRetryBudget = 2
)
