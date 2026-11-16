package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalidPolicy = errors.New("invalid rollout policy")
	ErrPriorRequired = errors.New("validated prior is required")
)

type Policy struct {
	CanaryInstances             int
	MaxAffectedInstances        int
	AggregateRegressionLimitPct float64
	RequireLocalReverification  bool
	Sources                     []string
}
type Instance struct {
	ID, Class string
	Tags      []string
}
type Prior struct {
	EvidenceID, ValidatedInstanceID string
	Intent                          json.RawMessage
}
type Request struct {
	Prior     Prior
	Instances []Instance
	Policy    Policy
}
type Reverification struct {
	Eligible                bool
	Reason, LocalEvidenceID string
}
type AppliedChange struct{ Handle string }
type Outcome struct {
	RegressionPct float64
	EvidenceID    string
	// Failed reports a failed verification of the change on the instance.
	Failed bool
	// Pending reports a verification with no verdict by its deadline: it
	// is neither a pass nor a failure, and the instance's own monitor
	// still owns the change, so the rollout halts without a rollback.
	Pending bool
	Detail  string
	// EffectPct is the measured effect RegressionPct holds (lab-03b): the
	// member's own v2 effect, or its change net of ControlPct, the
	// untreated members' median change; CILowPct and CIHighPct are its v2
	// interval. Nil with EffectMethod flag_only: the flag alone decided.
	EffectPct, CILowPct, CIHighPct, ControlPct *float64
	EffectMethod                               string
}
type InstanceResult struct {
	Status        string
	EvidenceID    string
	RegressionPct float64
	Detail        string
	// EffectPct, ControlPct and EffectMethod are the outcome's (lab-03b).
	EffectPct, ControlPct *float64
	EffectMethod          string
}
type Result struct {
	Halted                 bool
	HaltReason             string
	AppliedInstances       int
	CanaryInstanceIDs      []string
	AggregateRegressionPct float64
	Instances              map[string]InstanceResult
	// RolledBack counts instances rolled back after a halt;
	// RollbackErrors names the ones that could not be.
	RolledBack     int
	RollbackErrors []string
}

// RunRecord is one sage.rollout_run row: a fleet canary of a validated
// prior and how far it got.
type RunRecord struct {
	EvidenceID             string    `json:"id"`
	SourceInstance         string    `json:"source_database"`
	PriorEvidenceID        string    `json:"prior_evidence_id"`
	Policy                 Policy    `json:"policy"`
	State                  string    `json:"state"`
	HaltReason             string    `json:"halt_reason,omitempty"`
	AppliedInstances       int       `json:"applied_instances"`
	CanaryInstanceIDs      []string  `json:"canary_databases,omitempty"`
	AggregateRegressionPct float64   `json:"aggregate_regression_pct"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
	// Family, Class and StartedBy describe a fleet canary of a remediation.
	Family    string `json:"family,omitempty"`
	Class     string `json:"class,omitempty"`
	StartedBy string `json:"started_by,omitempty"`
	// Targets are the databases the run was asked to change, in order.
	Targets []string `json:"targets,omitempty"`
}

// Instance statuses.
const (
	StatusApplied             = "applied"
	StatusNotLocallyVerified  = "not_locally_verified"
	StatusRolledBack          = "rolled_back"
	StatusRollbackFailed      = "rollback_failed"
	StatusRollbackUnavailable = "rollback_unavailable"
	// StatusVerificationPending: applied, verdict still pending at its deadline.
	StatusVerificationPending = "verification_pending"
	// StatusFailed: the instance's re-verification or apply returned an error.
	StatusFailed = "failed"
	// StatusAppliedUnverified: changed, then a rollout error, and the
	// rollback failed too; the instance keeps a change no verdict judged.
	StatusAppliedUnverified = "applied_unverified"
	// StatusApplying: re-verified; the canary is changing the database.
	StatusApplying = "applying"
	// StatusAwaitingVerdict: changed; the database's own verdict is open.
	StatusAwaitingVerdict = "awaiting_verdict"
	// StatusInterrupted: the run stopped (a sidecar restart) while the
	// database was being changed or awaited its verdict. Any change it
	// made stays with that database's own post-action monitor.
	StatusInterrupted = "interrupted"
)

// StateInterrupted is a run a stopped sidecar left in canary; it is
// final and never resumed. HaltSidecarRestart is its halt reason.
const (
	StateInterrupted   = "interrupted"
	HaltSidecarRestart = "sidecar_restart"
)

// ErrRunInterrupted refuses a write for a run that is no longer in
// canary: another sidecar marked it interrupted (or it never existed).
var ErrRunInterrupted = errors.New("the canary run is no longer running")

// InterruptedRun is a run the startup sweep marked interrupted, with the
// instances it was changing or measuring when it stopped (Changing) and
// those it had changed with a verdict recorded, which keep the change
// because an interrupted run rolls nothing back (Kept).
type InterruptedRun struct {
	Run      RunRecord
	Changing []InstanceRecord
	Kept     []InstanceRecord
}

// HaltVerificationPending halts a rollout whose instance had no verdict
// by its deadline; it rolls nothing back.
const HaltVerificationPending = "verification_pending"

// Rollbacker undoes an applied change; an Applier that implements it lets
// a halted rollout roll back every instance it changed.
type Rollbacker interface {
	Rollback(context.Context, Instance, AppliedChange) error
}
type ReVerifier interface {
	Reverify(context.Context, Instance, Prior) (Reverification, error)
}
type Applier interface {
	Apply(context.Context, Instance, Reverification) (AppliedChange, error)
	Measure(context.Context, Instance, AppliedChange) (Outcome, error)
}
