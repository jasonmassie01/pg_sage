package wal

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestClassifyKeepsHealthyActiveLogicalSlot(t *testing.T) {
	decision, err := Classify(context.Background(), healthyLogicalSlot(), testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationHealthyActive, ActionKeep)
}

func TestClassifyKeepsHealthyActivePhysicalSlot(t *testing.T) {
	evidence := healthyLogicalSlot()
	evidence.SlotType = SlotTypePhysical
	evidence.SlotName = "standby_west"
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationHealthyActive, ActionKeep)
}

func TestClassifyBoundsLaggingActiveSlots(t *testing.T) {
	for _, slotType := range []SlotType{SlotTypeLogical, SlotTypePhysical} {
		t.Run(string(slotType), func(t *testing.T) {
			evidence := healthyLogicalSlot()
			evidence.SlotType = slotType
			evidence.RetainedWALBytes = 21 * gib
			decision, err := Classify(context.Background(), evidence, testPolicy())
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			requireDecision(t, decision, ClassificationLaggingActive, ActionBound)
		})
	}
}

func TestClassifyParksInactiveRecentSlotWithoutPressure(t *testing.T) {
	evidence := healthyLogicalSlot()
	evidence.Active = false
	evidence.InactiveFor = time.Hour
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationInactiveRecent, ActionPark)
}

func TestClassifyBoundsInactiveRecentSlotUnderWALPressure(t *testing.T) {
	evidence := healthyLogicalSlot()
	evidence.Active = false
	evidence.InactiveFor = time.Hour
	evidence.RetainedWALBytes = 21 * gib
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationInactiveRecent, ActionBound)
}

func TestClassifyInactiveAloneIsNeverSufficientToDrop(t *testing.T) {
	evidence := abandonedLogicalSlot()
	evidence.InactiveFor = 30 * 24 * time.Hour
	evidence.RetainedWALBytes = gib
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationInactiveRecent, ActionPark)
}

func TestClassifyDropsOnlyWithEveryPositiveSafetyPredicate(t *testing.T) {
	decision, err := Classify(context.Background(), abandonedLogicalSlot(), testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationAbandoned, ActionDrop)
	for _, token := range []string{"owner", "allowlist", "retained", "registry"} {
		if !strings.Contains(decision.Reason, token) {
			t.Errorf("drop reason %q does not record %q evidence", decision.Reason, token)
		}
	}
}

func TestClassifyRequiresRetainedWALAboveBoundaryForDrop(t *testing.T) {
	evidence := abandonedLogicalSlot()
	evidence.DiskCapacityKnown = false
	evidence.RetainedWALBytes = testPolicy().RetainedWALBytesThreshold
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationInactiveRecent, ActionPark)
	evidence.RetainedWALBytes++
	decision, err = Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify above threshold: %v", err)
	}
	requireDecision(t, decision, ClassificationAbandoned, ActionDrop)
}

func TestClassifyUsesDiskFractionAsRetainedWALPredicate(t *testing.T) {
	evidence := abandonedLogicalSlot()
	evidence.RetainedWALBytes = 11 * gib
	evidence.DiskCapacityBytes = 100 * gib
	policy := testPolicy()
	policy.RetainedWALBytesThreshold = 50 * gib
	decision, err := Classify(context.Background(), evidence, policy)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationAbandoned, ActionDrop)
}

func TestClassifyDropPredicatesFailClosedIndividually(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SlotEvidence, *Policy)
	}{
		{name: "drop not opted in", mutate: func(_ *SlotEvidence, p *Policy) {
			p.AllowDrop = false
		}},
		{name: "owner tag absent", mutate: func(e *SlotEvidence, _ *Policy) {
			e.OwnerTagKnown = false
			e.OwnerTag = ""
		}},
		{name: "owner not allowlisted", mutate: func(e *SlotEvidence, _ *Policy) {
			e.OwnerTag = "production-cdc"
		}},
		{name: "registry unknown", mutate: func(e *SlotEvidence, _ *Policy) {
			e.RegistryEvidenceKnown = false
		}},
		{name: "registered consumer", mutate: func(e *SlotEvidence, _ *Policy) {
			e.ConsumerRegistered = true
		}},
		{name: "recent activity", mutate: func(e *SlotEvidence, _ *Policy) {
			e.WasActiveWithinAbandonWindow = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := abandonedLogicalSlot()
			policy := testPolicy()
			test.mutate(&evidence, &policy)
			decision, err := Classify(context.Background(), evidence, policy)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if decision.Action == ActionDrop {
				t.Fatalf("unsafe evidence produced drop: %#v", decision)
			}
			requireDecision(t, decision, ClassificationAbandoned, ActionBound)
		})
	}
}

func TestClassifyNeverDropsPhysicalSlot(t *testing.T) {
	evidence := abandonedLogicalSlot()
	evidence.SlotType = SlotTypePhysical
	evidence.SlotName = "standby_west"
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationAbandoned, ActionBound)
}

func TestClassifyUnknownEvidenceFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SlotEvidence)
	}{
		{name: "last activity unknown", mutate: func(e *SlotEvidence) {
			e.LastActivityKnown = false
		}},
		{name: "retained WAL unknown", mutate: func(e *SlotEvidence) {
			e.RetainedWALKnown = false
		}},
		{name: "zero disk capacity", mutate: func(e *SlotEvidence) {
			e.DiskCapacityBytes = 0
			e.RetainedWALBytes = 5 * gib
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := abandonedLogicalSlot()
			test.mutate(&evidence)
			decision, err := Classify(context.Background(), evidence, testPolicy())
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if decision.Action == ActionDrop {
				t.Fatalf("unknown evidence produced drop: %#v", decision)
			}
		})
	}
}

func TestClassifyFlappingSlotIsNeverDropped(t *testing.T) {
	evidence := abandonedLogicalSlot()
	evidence.WasActiveWithinAbandonWindow = true
	decision, err := Classify(context.Background(), evidence, testPolicy())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	requireDecision(t, decision, ClassificationAbandoned, ActionBound)
}
