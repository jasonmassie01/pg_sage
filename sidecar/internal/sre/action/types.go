package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE M5 (AI-SRE-SPEC R1.1): a concluded investigation whose root is
// one identifiable active backend proposes an evidence-matched cancel.
// A proposal is a durable recommendation linked to its investigation; it
// never executes by itself. Requesting execution queues it in the
// existing approval flow; a human approval runs it through the executor
// and the policy gate after a fresh identity recheck, and recovery is
// verified over fresh samples. Every step lands on the investigation's
// hash chain.

// ActionClass names a Sage SRE action class.
type ActionClass string

// ActionCancelBackend cancels one evidence-matched backend's statement.
const ActionCancelBackend ActionClass = "cancel_backend"

// ProposalState is a proposal's lifecycle state.
type ProposalState string

// Proposal states.
const (
	ProposalIneligible ProposalState = "ineligible"
	ProposalProposed   ProposalState = "proposed"
	ProposalRequested  ProposalState = "requested"
	ProposalExecuting  ProposalState = "executing"
	ProposalExecuted   ProposalState = "executed"
	ProposalRefused    ProposalState = "refused"
	ProposalFailed     ProposalState = "failed"
	ProposalUncertain  ProposalState = "uncertain"
	ProposalDenied     ProposalState = "denied"
	ProposalExpired    ProposalState = "expired"
)

// RecoveryState is a recovery verification's state (Codex §7).
type RecoveryState string

// Recovery states; RecoveryNone means no verification was started.
const (
	RecoveryNone         RecoveryState = ""
	RecoveryObserving    RecoveryState = "observing"
	RecoveryRecovered    RecoveryState = "recovered"
	RecoveryNotRecovered RecoveryState = "not_recovered"
	RecoveryInconclusive RecoveryState = "inconclusive"
)

// Recovery attributions: who changed the incident.
const (
	AttributionSage     = "pg_sage"
	AttributionExternal = "external"
	AttributionUnknown  = "unknown"
)

// ActionReason is why a proposal was not made, or an action not run.
type ActionReason string

// Reasons.
const (
	ReasonNotConcluded         ActionReason = "not_concluded"
	ReasonUnsupportedFamily    ActionReason = "unsupported_family"
	ReasonUnsupportedRoot      ActionReason = "unsupported_root"
	ReasonIdleInTransaction    ActionReason = "idle_in_transaction"
	ReasonRootNotBackend       ActionReason = "root_not_a_backend"
	ReasonShortContention      ActionReason = "short_transaction_contention"
	ReasonTargetNotActive      ActionReason = "target_not_active"
	ReasonNoWaiters            ActionReason = "no_waiters"
	ReasonLockEvidenceMissing  ActionReason = "lock_evidence_missing"
	ReasonEvidenceInconsistent ActionReason = "evidence_inconsistent"
	ReasonTargetGone           ActionReason = "target_gone"
	ReasonTargetChanged        ActionReason = "target_changed"
	ReasonProtected            ActionReason = "protected_backend"
	ReasonReplica              ActionReason = "replica"
	ReasonNotBlocking          ActionReason = "not_blocking"
	ReasonOtherDatabase        ActionReason = "other_database"
	ReasonTargetProbeFailed    ActionReason = "target_probe_failed"
	ReasonEvidenceStale        ActionReason = "evidence_stale"
	ReasonPolicyWithheld       ActionReason = "policy_withheld"
	ReasonExecutionError       ActionReason = "execution_error"
)

// Action errors, distinguishable by callers (API, MCP, ChatOps).
var (
	ErrProposalNotFound = errors.New("action proposal not found")
	ErrProposalState    = errors.New("action proposal state does not allow this")
	ErrPolicyBlocked    = errors.New("policy withholds the action")
	ErrHandoffBlocked   = errors.New(
		"action handoff blocked: investigation metadata is not durable")
)

// BackendTarget is the identity and state of the proposal's target
// session when it was sampled (never query text).
type BackendTarget struct {
	PID          int32     `json:"pid"`
	BackendStart time.Time `json:"backend_start"`
	QueryStart   time.Time `json:"query_start"`
	XactStart    time.Time `json:"xact_start"`
	Database     string    `json:"database"`
	User         string    `json:"user"`
	State        string    `json:"state"`
	BackendType  string    `json:"backend_type"`
	QueryHash    string    `json:"query_hash"`
	QueryID      int64     `json:"query_id,string"`
	Blocking     int       `json:"blocking"`
	InRecovery   bool      `json:"in_recovery"`
	// StatementIsTransaction: the statement is its whole transaction, so
	// a cancel ends the transaction and releases its locks.
	StatementIsTransaction bool      `json:"statement_is_transaction"`
	ObservedAt             time.Time `json:"observed_at"`
}

// Identity is the executor's view of the target.
func (t BackendTarget) Identity() executor.BackendIdentity {
	return executor.BackendIdentity{PID: t.PID, BackendStart: t.BackendStart,
		QueryStart: t.QueryStart, Database: t.Database, User: t.User,
		QueryHash: t.QueryHash, QueryID: t.QueryID}
}

// Waiter is one session that waited behind the target (a recovery
// baseline entry).
type Waiter struct {
	PID          int32     `json:"pid"`
	BackendStart time.Time `json:"backend_start"`
}

// Baseline is the blocking the evidence showed before any action.
type Baseline struct {
	Waiting int      `json:"waiting"`
	Waiters []Waiter `json:"waiters"`
}

// PolicyVerdict is the standing gate's verdict for an approved run.
type PolicyVerdict struct {
	Decision string `json:"decision"`
	RiskTier string `json:"risk_tier,omitempty"`
	Reason   string `json:"blocked_reason,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func verdictOf(d executor.ActionPolicyDecision) PolicyVerdict {
	return PolicyVerdict{Decision: d.Decision, RiskTier: d.RiskTier,
		Reason: d.BlockedReason, Detail: d.Detail}
}

func (v PolicyVerdict) allows() bool { return v.Decision == executor.PolicyDecisionExecute }

func (v PolicyVerdict) String() string {
	if v.Detail != "" {
		return fmt.Sprintf("%s (%s)", v.Reason, v.Detail)
	}
	return v.Reason
}

// ActionConfig configures the action service.
type ActionConfig struct {
	// Proposals turns automatic proposals on; an operator may always
	// propose explicitly.
	Proposals bool
	// RequestApproval queues an automatic proposal the policy would allow
	// and notifies ChatOps channels.
	RequestApproval       bool
	ApprovalTTL           time.Duration
	MaxEvidenceAge        time.Duration
	RecoveryInterval      time.Duration
	RecoverySamples       int
	RecoveryDeadline      time.Duration
	PollInterval          time.Duration
	ProtectedRoles        []string
	ProtectedApplications []string
}

// DefaultActionConfig is the AI-DBA default: proposals and approval
// requests on, execution only after a human approval.
func DefaultActionConfig() ActionConfig {
	return ActionConfig{Proposals: true, RequestApproval: true,
		ApprovalTTL: 15 * time.Minute, MaxEvidenceAge: executor.MaxBackendEvidenceAge,
		RecoveryInterval: 40 * time.Second, RecoverySamples: 3,
		RecoveryDeadline: 30 * time.Minute, PollInterval: 5 * time.Second}
}

// Validate checks the bounds; the evidence age can only be tightened.
func (c ActionConfig) Validate() error {
	switch {
	case c.ApprovalTTL <= 0:
		return fmt.Errorf("%w: approval TTL %s", sre.ErrInvalidRequest, c.ApprovalTTL)
	case c.MaxEvidenceAge <= 0 || c.MaxEvidenceAge > executor.MaxBackendEvidenceAge:
		return fmt.Errorf("%w: evidence age %s outside (0, %s]", sre.ErrInvalidRequest,
			c.MaxEvidenceAge, executor.MaxBackendEvidenceAge)
	case c.RecoveryInterval <= 0 || c.RecoverySamples < 3 || c.RecoveryDeadline <= 0:
		return fmt.Errorf("%w: recovery needs >= 3 samples, a positive interval and "+
			"deadline", sre.ErrInvalidRequest)
	case c.PollInterval <= 0:
		return fmt.Errorf("%w: poll interval %s", sre.ErrInvalidRequest, c.PollInterval)
	}
	return nil
}

// BackendCanceller is the executor half of an approved cancel.
type BackendCanceller interface {
	CancelBackend(ctx context.Context, req executor.BackendCancel) (int64, error)
	PreviewBackendCancel(ctx context.Context, isReplica bool) executor.ActionPolicyDecision
}

// recoveryRecorder closes the executor's verification of an action with
// the recovery verdict (the real executor implements it).
type recoveryRecorder interface {
	RecordRecoveryVerdict(ctx context.Context, actionID int64, verdict, reason string) error
}

// ApprovalRequest is what a ChatOps notification carries.
type ApprovalRequest struct {
	Database        string
	ProposalID      sre.UUID
	InvestigationID sre.UUID
	QueueID         int
	Title           string
	Summary         string
	Risk            string
	ExpiresAt       time.Time
}

// ApprovalNotifier sends approval requests to chat.
type ApprovalNotifier interface {
	ApprovalRequested(ctx context.Context, r ApprovalRequest) error
}
