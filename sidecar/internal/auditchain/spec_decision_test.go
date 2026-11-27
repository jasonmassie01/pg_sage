package auditchain

import (
	"strings"
	"testing"
)

// No concurrent access tests: specs are immutable package values.

func exprsHave(exprs []string, column string) bool {
	for _, e := range exprs {
		if strings.Contains(e, "%[1]s."+column+")") || strings.HasSuffix(e, "%[1]s."+column) {
			return true
		}
	}
	return false
}

// audit-11: the decision's authority is sealed, its outcome is state, and
// the repeat counters stay out of the chain.
func TestDecisionLedgerSpecSplitsAuthorityFromState(t *testing.T) {
	s := DecisionLedger
	if s.Chain != "decision" || s.Table != "sage.decision" || s.Version != 1 || !s.Optional {
		t.Fatalf("decision spec = %+v", s)
	}
	if !strings.HasSuffix(s.Sealed[0], ".id") {
		t.Fatalf("sealed columns must start with the row id: %v", s.Sealed)
	}
	for _, c := range []string{"database_id", "feature", "intent", "target_objects",
		"policy_version", "policy_doc_sha256", "risk_tier", "evidence_id", "fingerprint",
		"principal_id", "task_id", "artifact_hash", "created_at"} {
		if !exprsHave(s.Sealed, c) || exprsHave(s.State, c) {
			t.Errorf("%s must be sealed (and only sealed)", c)
		}
	}
	for _, c := range []string{"verdict", "reason", "evidence", "guardrails",
		"off_window_ok", "deadline_kind", "deadline_hard_at", "queue_id", "action_log_id",
		"resolved_at"} {
		if !exprsHave(s.State, c) || exprsHave(s.Sealed, c) {
			t.Errorf("%s must be state (and only state)", c)
		}
	}
	for _, c := range []string{"repeat_count", "last_seen_at"} {
		if exprsHave(s.Sealed, c) || exprsHave(s.State, c) {
			t.Errorf("%s is telemetry and must stay unchained", c)
		}
	}
}

func TestPolicyApprovalsSpec(t *testing.T) {
	s := PolicyApprovals
	if s.Chain != "policy_approvals" || s.Table != "sage.policy_approvals" ||
		s.Version != 1 || !s.Optional || !strings.HasSuffix(s.Sealed[0], ".id") {
		t.Fatalf("policy_approvals spec = %+v", s)
	}
	for _, c := range []string{"policy_id", "approver", "approver_user_id", "reason",
		"single_operator", "decided_at"} {
		if !exprsHave(s.Sealed, c) {
			t.Errorf("%s must be sealed", c)
		}
	}
	if len(s.State) != 1 || !exprsHave(s.State, "review_status") {
		t.Fatalf("state = %v, want review_status only", s.State)
	}
}

// The new chains are appended: the existing chains keep their order,
// version and columns (their hashes are pinned by golden_db_test.go).
func TestExistingChainsUnchanged(t *testing.T) {
	want := []string{"action_log", "auth_audit", "config_audit", "guard_query_audit",
		"pgaudit_events", "fact_events", "decision", "policy_approvals"}
	specs := Specs()
	if len(specs) < len(want) {
		t.Fatalf("%d specs, want the %d released chains first", len(specs), len(want))
	}
	for i, s := range specs[:len(want)] {
		if s.Chain != want[i] || s.Version != 1 {
			t.Fatalf("spec %d = %s v%d, want %s v1", i, s.Chain, s.Version, want[i])
		}
		if _, ok := SealedColumnTypes[SpecKey{Chain: s.Chain, Version: 1}]; !ok {
			t.Fatalf("%s has no column type registry entry", s.Chain)
		}
	}
	if len(ActionLog.Sealed) != 11 || len(ActionLog.State) != 8 {
		t.Fatalf("action_log columns moved: %d sealed, %d state", len(ActionLog.Sealed),
			len(ActionLog.State))
	}
	if !strings.Contains(MigrationSQL(), "sage.audit_chain_link_decision()") ||
		!strings.Contains(MigrationSQL(), "sage.audit_chain_link_policy_approvals()") {
		t.Fatal("the migration does not install the decision and approval chains")
	}
}
