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
}

type RuntimeState struct {
	ExecutorEnabled bool
	EmergencyStop   bool
	IsReplica       bool
	TrustLevel      string
	ExecutionMode   string
}

const (
	TrustObservation  = "observation"
	TrustAdvisory     = "advisory"
	TrustAutonomous   = "autonomous"
	ExecutionAuto     = "auto"
	ExecutionApproval = "approval"
	ExecutionManual   = "manual"
)

type LimitUsage struct {
	StorageBytes                 int64
	RowsRewritten                int64
	TablesInWindow               int64
	SelfInitiatedChangesInWindow int64
}

type Gate interface {
	Authorize(context.Context, ActionRequest) Decision
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
}

var (
	ErrPolicyNotFound = errors.New("policy not found")
	ErrPolicyInvalid  = errors.New("policy invalid")
)
