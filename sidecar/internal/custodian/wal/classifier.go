package wal

import (
	"context"
	"strings"
	"time"
)

type SlotType string

const (
	SlotTypeLogical  SlotType = "logical"
	SlotTypePhysical SlotType = "physical"
)

type Classification string

const (
	ClassificationHealthyActive  Classification = "healthy_active"
	ClassificationLaggingActive  Classification = "lagging_active"
	ClassificationInactiveRecent Classification = "inactive_recent"
	ClassificationAbandoned      Classification = "abandoned"
)

type Action string

const (
	ActionKeep  Action = "keep"
	ActionBound Action = "bound"
	ActionPark  Action = "park"
	ActionDrop  Action = "drop"
)

type Policy struct {
	AbandonAfter              time.Duration
	RetainedWALBytesThreshold int64
	RetainedWALDiskPctCeiling float64
	AllowDrop                 bool
	DropOwnerAllowlist        []string
}
type SlotEvidence struct {
	SlotName                     string
	SlotType                     SlotType
	Active                       bool
	InactiveFor                  time.Duration
	LastActivityKnown            bool
	WasActiveWithinAbandonWindow bool
	RetainedWALKnown             bool
	RetainedWALBytes             int64
	DiskCapacityKnown            bool
	DiskCapacityBytes            int64
	RegistryEvidenceKnown        bool
	ConsumerRegistered           bool
	OwnerTagKnown                bool
	OwnerTag                     string
}
type Decision struct {
	Classification Classification
	Action         Action
	Reason         string
}

func DefaultPolicy() Policy {
	return Policy{AbandonAfter: 24 * time.Hour,
		RetainedWALBytesThreshold: 10 << 30, RetainedWALDiskPctCeiling: 10}
}

func Classify(ctx context.Context, evidence SlotEvidence, policy Policy) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{Action: ActionPark}, err
	}
	if evidence.Active {
		if retainedPressure(evidence, policy) {
			return Decision{ClassificationLaggingActive, ActionBound, "retained WAL pressure"}, nil
		}
		return Decision{ClassificationHealthyActive, ActionKeep, "slot is active and bounded"}, nil
	}
	abandoned := evidence.LastActivityKnown && evidence.InactiveFor > policy.AbandonAfter
	if !abandoned || !retainedPressure(evidence, policy) {
		action := ActionPark
		if retainedPressure(evidence, policy) {
			action = ActionBound
		}
		return Decision{ClassificationInactiveRecent, action, "inactive evidence is insufficient"}, nil
	}
	decision := Decision{ClassificationAbandoned, ActionBound,
		"bound retained WAL because drop proof is incomplete"}
	if dropProven(evidence, policy) {
		decision.Action = ActionDrop
		decision.Reason = "owner tag and allowlist, retained WAL, registry unregistered proof"
	}
	return decision, nil
}

func retainedPressure(evidence SlotEvidence, policy Policy) bool {
	if !evidence.RetainedWALKnown {
		return false
	}
	bytesExceeded := policy.RetainedWALBytesThreshold > 0 &&
		evidence.RetainedWALBytes > policy.RetainedWALBytesThreshold
	diskExceeded := evidence.DiskCapacityKnown && evidence.DiskCapacityBytes > 0 &&
		float64(evidence.RetainedWALBytes)*100/float64(evidence.DiskCapacityBytes) >
			policy.RetainedWALDiskPctCeiling
	return bytesExceeded || diskExceeded
}

func dropProven(evidence SlotEvidence, policy Policy) bool {
	return policy.AllowDrop && evidence.SlotType == SlotTypeLogical &&
		evidence.LastActivityKnown && !evidence.WasActiveWithinAbandonWindow &&
		evidence.RetainedWALKnown && retainedPressure(evidence, policy) &&
		evidence.RegistryEvidenceKnown && !evidence.ConsumerRegistered &&
		evidence.OwnerTagKnown && ownerAllowed(evidence.OwnerTag, policy.DropOwnerAllowlist)
}

func ownerAllowed(owner string, allowlist []string) bool {
	for _, allowed := range allowlist {
		if strings.EqualFold(strings.TrimSpace(owner), strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}
