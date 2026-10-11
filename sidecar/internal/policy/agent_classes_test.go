package policy

import (
	"context"
	"testing"
)

// Agent change classes (spec §6.2.2 A6, §6.3) and the typed agent
// role contracts.

func TestAgentChangeClasses_SixInSpecOrder(t *testing.T) {
	want := []ChangeClass{"agent_access", "agent_data_write", "agent_schema_change",
		"agent_maintenance", "agent_sandbox", "agent_estate"}
	got := AgentChangeClasses()
	if len(got) != len(want) {
		t.Fatalf("classes = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("class %d = %q, want %q", i, got[i], want[i])
		}
		if !IsAgentChangeClass(want[i]) {
			t.Fatalf("%q not recognized", want[i])
		}
	}
	if IsAgentChangeClass(ChangeIndex) || IsAgentChangeClass("agent") {
		t.Fatal("a non-agent class was recognized")
	}
}

func TestAgentChangeClasses_DefaultDocumentAllowsThemOnlyWithApproval(t *testing.T) {
	for _, doc := range []Document{StaffedProfile(), UnattendedProfile()} {
		if err := ValidateDocument(doc); err != nil {
			t.Fatalf("%s: %v", doc.Profile, err)
		}
		for _, class := range AgentChangeClasses() {
			if !containsChangeClass(doc.AllowedChangeClasses, class) {
				t.Fatalf("%s does not allow %s", doc.Profile, class)
			}
			if !containsChangeClass(doc.ApprovalRequiredClasses, class) {
				t.Fatalf("%s does not require approval for %s", doc.Profile, class)
			}
		}
		if !containsChangeClass(doc.ApprovalRequiredClasses, ChangeOnlineMigration) {
			t.Fatalf("%s lost online_migration from approval-required", doc.Profile)
		}
		if containsChangeClass(doc.ApprovalRequiredClasses, ChangeIndex) {
			t.Fatalf("%s now requires approval for index", doc.Profile)
		}
	}
}

func guardRoleRequest(actionType string, approved bool) ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: actionType, RiskTier: RiskModerate,
			RollbackClass: RollbackReversible},
		InternalControl: true, Feature: string(ChangeAgentAccess), OperatorApproved: approved,
		TargetObjs: []string{"role:sage_agentb_abcdefghij"},
	}
}

func TestGuardRoleContract_TypedInternalSkipsSQLValidation(t *testing.T) {
	var calls []string
	gate := newTestGate(t, gateFixture{calls: &calls})
	for _, at := range []string{"guard_role_ensure", "guard_role_retire"} {
		assertDecision(t, gate.Authorize(context.Background(), guardRoleRequest(at, true)),
			VerdictExecute, ReasonOperatorApproved)
	}
	assertNotCalled(t, calls, "validate_sql")
}

func TestGuardRoleContract_UnapprovedQueuesForAHuman(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	assertDecision(t, gate.Authorize(context.Background(),
		guardRoleRequest("guard_role_ensure", false)), VerdictQueueApproval,
		ReasonApprovalRequired)
}

func TestGuardRoleContract_SQLOrUnknownTypeIsNotTrusted(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	withSQL := guardRoleRequest("guard_role_ensure", true)
	withSQL.SQL = "CREATE ROLE x SUPERUSER"
	assertDecision(t, gate.Authorize(context.Background(), withSQL), VerdictPark,
		ReasonNoTypedContract)
	unknown := guardRoleRequest("guard_role_superuser", true)
	assertDecision(t, gate.Authorize(context.Background(), unknown), VerdictPark,
		ReasonNoTypedContract)
	notInternal := guardRoleRequest("guard_role_ensure", true)
	notInternal.InternalControl = false
	assertDecision(t, gate.Authorize(context.Background(), notInternal), VerdictPark,
		ReasonNoTypedContract)
}

func TestGuardRoleContract_StoredPolicyWithoutClassBlocks(t *testing.T) {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.AllowedChangeClasses = []ChangeClass{ChangeIndex}
	doc.ApprovalRequiredClasses = nil
	gate := newTestGate(t, gateFixture{policy: doc})
	assertDecision(t, gate.Authorize(context.Background(),
		guardRoleRequest("guard_role_ensure", true)), VerdictBlocked,
		ReasonChangeClassNotAllowed)
}

func TestGuardRoleContract_ObservationTrustRecordsOnly(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.TrustLevel = TrustObservation
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	got := gate.Authorize(context.Background(), guardRoleRequest("guard_role_ensure", true))
	if got.Verdict != VerdictObserveOnly {
		t.Fatalf("verdict = %s, want observe_only", got.Verdict)
	}
}
