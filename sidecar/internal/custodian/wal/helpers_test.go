package wal

import "time"

const gib = int64(1 << 30)

func testPolicy() Policy {
	return Policy{
		AbandonAfter:              24 * time.Hour,
		RetainedWALBytesThreshold: 10 * gib,
		RetainedWALDiskPctCeiling: 10,
		AllowDrop:                 true,
		DropOwnerAllowlist:        []string{"ephemeral-agent"},
	}
}

func healthyLogicalSlot() SlotEvidence {
	return SlotEvidence{
		SlotName:              "orders_cdc",
		SlotType:              SlotTypeLogical,
		Active:                true,
		LastActivityKnown:     true,
		RetainedWALKnown:      true,
		RetainedWALBytes:      gib,
		DiskCapacityKnown:     true,
		DiskCapacityBytes:     200 * gib,
		RegistryEvidenceKnown: true,
		OwnerTagKnown:         true,
		OwnerTag:              "ephemeral-agent",
	}
}

func abandonedLogicalSlot() SlotEvidence {
	evidence := healthyLogicalSlot()
	evidence.Active = false
	evidence.InactiveFor = 25 * time.Hour
	evidence.RetainedWALBytes = 21 * gib
	evidence.ConsumerRegistered = false
	return evidence
}

func requireDecision(
	testingT interface {
		Helper()
		Fatalf(string, ...any)
	},
	decision Decision,
	wantClass Classification,
	wantAction Action,
) {
	testingT.Helper()
	if decision.Classification != wantClass || decision.Action != wantAction {
		testingT.Fatalf(
			"decision = %#v, want classification=%q action=%q",
			decision, wantClass, wantAction,
		)
	}
}
