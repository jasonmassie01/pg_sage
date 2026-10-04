package policy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Verdict string

const (
	VerdictExecute       Verdict = "execute"
	VerdictQueueApproval Verdict = "queue_approval"
	VerdictPark          Verdict = "park"
	VerdictBlocked       Verdict = "blocked"
	VerdictObserveOnly   Verdict = "observe_only"
)

type RiskTier string

const (
	RiskReadOnly RiskTier = "read_only"
	RiskSafe     RiskTier = "safe"
	RiskModerate RiskTier = "moderate"
	RiskHigh     RiskTier = "high"
)

type Guardrail string

const GuardrailApprovalRequired Guardrail = "approval_required"

type Reason string

const (
	ReasonAuthorized               Reason = "authorized"
	ReasonExecutorDisabled         Reason = "executor_disabled"
	ReasonEmergencyStop            Reason = "emergency_stop"
	ReasonReplicaMutation          Reason = "replica_mutation"
	ReasonNoTypedContract          Reason = "no_typed_contract"
	ReasonObserveOnly              Reason = "observe_only"
	ReasonPolicyUnavailable        Reason = "policy_unavailable"
	ReasonApprovalRequired         Reason = "approval_required"
	ReasonUnknownGuardrail         Reason = "unknown_guardrail"
	ReasonBudgetExceeded           Reason = "budget_exceeded"
	ReasonBlastRadiusExceeded      Reason = "blast_radius_exceeded"
	ReasonRateLimitExceeded        Reason = "rate_limit_exceeded"
	ReasonOutsideMaintenanceWindow Reason = "outside_maintenance_window"
	ReasonDeadlineOverride         Reason = "deadline_override"
	ReasonUnknownRiskTier          Reason = "unknown_risk_tier"
	ReasonChangeClassNotAllowed    Reason = "change_class_not_allowed"
	ReasonTrustRampNotSatisfied    Reason = "trust_ramp_not_satisfied"
	ReasonProviderUnsupported      Reason = "provider_unsupported"
	ReasonOperatorApproved         Reason = "operator_approved"
	ReasonUnknownTrustLevel        Reason = "unknown_trust_level"
	ReasonSQLValidationDegraded    Reason = "sql_validation_degraded"
	// ReasonRefusedByPolicy sends a self-initiated action that matches a
	// refusal_set token to a human; Decision.Detail names the token.
	ReasonRefusedByPolicy Reason = "refused_by_policy"
	// ReasonDDLConflict parks an action whose DDL lease overlaps another
	// writer; it is retried next cycle and is not a failure.
	ReasonDDLConflict Reason = "ddl_conflict"
)

// RollbackClass states how an action is undone. It mirrors the executor
// contract's rollback class.
type RollbackClass string

const (
	RollbackReversible       RollbackClass = "reversible"
	RollbackNoRollbackNeeded RollbackClass = "no_rollback_needed"
	RollbackNotApplicable    RollbackClass = "not_applicable"
	RollbackApplication      RollbackClass = "application_rollback"
	RollbackForwardFixOnly   RollbackClass = "forward_fix_only"
	RollbackNotReversible    RollbackClass = "not_reversible"
	// RollbackMitigationOnly: the action mitigates an incident and cannot be
	// undone, but it changes no data or schema (a backend cancel).
	RollbackMitigationOnly RollbackClass = "mitigation_only"
)

// DropKind classifies the object an action drops. Derivable objects
// (indexes) can be rebuilt from a recorded definition; non-derivable ones
// (tables, columns, constraints, sequences, schemas, replication slots)
// carry data or state that a definition cannot restore.
type DropKind string

const (
	DropDerivable    DropKind = "derivable"
	DropNonDerivable DropKind = "non_derivable"
)

type DeadlineKind string

const (
	DeadlineXID  DeadlineKind = "xid"
	DeadlineDisk DeadlineKind = "disk"
)

type Urgency string

const UrgencyCritical Urgency = "critical"

type DeadlineContext struct {
	Kind    DeadlineKind
	Urgency Urgency
	HardAt  time.Time
}

type ActionContract struct {
	ActionType string
	RiskTier   RiskTier
	Guardrails []Guardrail
	// ProviderSupport lists the providers that can run the action; empty
	// means every provider.
	ProviderSupport []string
	// RollbackClass is how the action is undone; empty declares nothing.
	RollbackClass RollbackClass
	// DropKind classifies a dropped object. Empty means the gate derives
	// it from the request SQL.
	DropKind DropKind
}

type ActionRequest struct {
	DatabaseID      *int64
	Contract        *ActionContract
	Arguments       json.RawMessage
	InternalControl bool
	SQL             string
	TargetObjs      []string
	Feature         string
	Deadline        *DeadlineContext
	Evidence        map[string]any
	IsReplica       bool
	// ExplainFamily asks Explain for an action family's readiness, where no
	// concrete SQL exists. Authorize ignores it and always validates SQL.
	ExplainFamily bool
	// OperatorApproved marks a request a human approved: tier, ramp,
	// execution mode and self-initiated usage limits no longer apply.
	OperatorApproved bool
	// OwnerDeclared marks a request that runs under an owner's explicit
	// declaration (a retention contract), which is its authority.
	OwnerDeclared bool
	// IncidentFamily names the Sage SRE incident family this action
	// remediates; the earned-autonomy ledger governs self-initiated
	// requests that carry one (M7).
	IncidentFamily string
	// EvidenceObservedAt is when the evidence behind the action was
	// observed; stale evidence downgrades earned autonomy.
	EvidenceObservedAt time.Time
	// LeaseHeld marks the re-authorization that follows this action's own
	// change lease, which is therefore not a concurrent writer.
	LeaseHeld bool
	// RevertsOwnChange marks the executor's own rollback of a change it
	// made. Only the executor's rollback path sets it; such a request is
	// not bound by the kind budgets (see BudgetBypassFor).
	RevertsOwnChange bool
}

type Decision struct {
	Verdict     Verdict
	RiskTier    RiskTier
	Reason      Reason
	Detail      string
	Guardrails  []Guardrail
	OffWindowOK bool
	EvidenceID  string
	DecisionID  int64
	// LockCeilingMS is the policy's lock_duration_ceiling_ms on an execute
	// verdict (0 = no ceiling). In-transaction DDL caps lock_timeout by it.
	LockCeilingMS int64
	// SerializeMode is the policy's serialize_mode on an execute verdict:
	// what a change lease conflict does (park, or wait in the lease queue).
	SerializeMode string
	// BudgetKind is the budget the request was charged to, and
	// RowsRewritten its own estimate of rows rewritten, when the gate
	// read usage (self-initiated, non-read-only requests).
	BudgetKind    BudgetKind
	RowsRewritten int64
}

// RuntimeState is the live authority snapshot for one authorization.
// Tier3Safe, Tier3Moderate, RampStart and InConfiguredWindow carry the
// sidecar config ceilings (trust.tier3_*, the trust ramp and
// trust.maintenance_window). Their zero values fail closed: a caller that
// does not supply them never gets autonomous safe/moderate execution.
type RuntimeState struct {
	ExecutorEnabled bool
	EmergencyStop   bool
	IsReplica       bool
	TrustLevel      string
	ExecutionMode   string
	Tier3Safe       bool
	Tier3Moderate   bool
	RampStart       time.Time
	// SafeRampAge and ModerateRampAge are trust.ramp_safe_hours and
	// trust.ramp_moderate_hours. Zero takes the spec ramp (8 / 31 days);
	// see rampAge for the floors.
	SafeRampAge        time.Duration
	ModerateRampAge    time.Duration
	InConfiguredWindow bool
	// Provider is the target's platform (cloud-sql, rds, ...); empty or
	// "self-managed" means plain postgres.
	Provider string
	// WindowConfigured reports whether trust.maintenance_window is set. An
	// unset window restricts autonomous moderate actions but not operator
	// approvals.
	WindowConfigured bool
	// SQLValidationDegraded reports a build without the parse-tree SQL
	// layer (no cgo). Mutations it would run unattended go to approval.
	SQLValidationDegraded bool
}

const (
	TrustObservation  = "observation"
	TrustAdvisory     = "advisory"
	TrustAutonomous   = "autonomous"
	ExecutionAuto     = "auto"
	ExecutionApproval = "approval"
	ExecutionManual   = "manual"
)

// LimitUsage is the rolling window of the request's budget kind if the
// request runs. Rows rewritten are one budget shared by every kind.
type LimitUsage struct {
	StorageBytes int64
	// RowsRewritten is the window's recorded rewrites plus the request's
	// own estimate, RequestRowsRewritten.
	RowsRewritten        int64
	RequestRowsRewritten int64
	// TablesInWindow is the distinct tables the window holds if the
	// request runs (the ones already touched plus the request's own).
	TablesInWindow               int64
	SelfInitiatedChangesInWindow int64
	// TablesFreeAt, ChangesFreeAt and RowsFreeAt are when the window next
	// frees a table, a change or rewritten rows (zero: nothing to free).
	TablesFreeAt  time.Time
	ChangesFreeAt time.Time
	RowsFreeAt    time.Time
}

type Gate interface {
	Authorize(context.Context, ActionRequest) Decision
}

// Explainer evaluates a request exactly as Authorize would without
// recording a decision, for operator-facing readiness and previews.
type Explainer interface {
	Explain(context.Context, ActionRequest) Decision
}

type GateConfig struct {
	Runtime                func(context.Context, ActionRequest) (RuntimeState, error)
	ValidateSQL            func(string) error
	Policy                 func(context.Context, ActionRequest) (Document, error)
	Usage                  func(context.Context, ActionRequest) (LimitUsage, error)
	WindowObserved         func()
	RecordDecision         func(context.Context, ActionRequest, Decision) (string, error)
	RecordDecisionDetailed func(context.Context, ActionRequest, Decision) (string, int64, error)
	Now                    func() time.Time
	// Autonomy is the earned-autonomy ledger (M7); nil leaves verdicts as
	// trust, mode, tiers and windows decide them.
	Autonomy AutonomyLimiter
	// Serialize, when set, runs the usage read and the decision record of
	// a budget-spending request in one transaction holding a lock shared
	// by every sidecar on the database. It returns the context Usage and
	// the recorder run under and done(commit), which ends the transaction.
	Serialize func(context.Context, ActionRequest) (context.Context, func(commit bool) error,
		error)
}

var (
	ErrPolicyNotFound = errors.New("policy not found")
	ErrPolicyInvalid  = errors.New("policy invalid")
)
