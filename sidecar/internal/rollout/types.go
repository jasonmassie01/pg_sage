package rollout

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrPolicyUnavailable = errors.New("rollout policy unavailable")
	ErrInvalidPolicy     = errors.New("invalid rollout policy")
	ErrPriorRequired     = errors.New("validated prior is required")
)

type Policy struct {
	CanaryInstances             int
	MaxAffectedInstances        int
	AggregateRegressionLimitPct float64
	RequireLocalReverification  bool
	Sources                     []string
}
type PolicyPatch struct {
	CanaryInstances             *int
	MaxAffectedInstances        *int
	AggregateRegressionLimitPct *float64
	RequireLocalReverification  *bool
}
type PolicySet struct {
	FleetDefault      *Policy
	ClassDefaults     map[string]PolicyPatch
	TagDefaults       map[string]PolicyPatch
	InstanceOverrides map[string]PolicyPatch
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
	Detail string
}
type InstanceResult struct {
	Status        string
	EvidenceID    string
	RegressionPct float64
	Detail        string
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

// Instance statuses.
const (
	StatusApplied             = "applied"
	StatusNotLocallyVerified  = "not_locally_verified"
	StatusRolledBack          = "rolled_back"
	StatusRollbackFailed      = "rollback_failed"
	StatusRollbackUnavailable = "rollback_unavailable"
)

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
