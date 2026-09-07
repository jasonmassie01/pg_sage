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
}
type InstanceResult struct {
	Status     string
	EvidenceID string
}
type Result struct {
	Halted                 bool
	HaltReason             string
	AppliedInstances       int
	CanaryInstanceIDs      []string
	AggregateRegressionPct float64
	Instances              map[string]InstanceResult
}
type ReVerifier interface {
	Reverify(context.Context, Instance, Prior) (Reverification, error)
}
type Applier interface {
	Apply(context.Context, Instance, Reverification) (AppliedChange, error)
	Measure(context.Context, Instance, AppliedChange) (Outcome, error)
}
