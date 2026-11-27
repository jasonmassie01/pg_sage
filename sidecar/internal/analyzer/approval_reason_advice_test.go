package analyzer

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// No concurrent access tests: excludedFromAdvice is a pure function.

// The approval reason is one of the detail keys the self-monitoring scan
// skips, so the two packages cannot drift apart.
func TestApprovalReasonIsAnExplanationKey(t *testing.T) {
	if !slices.Contains(selfmonitor.ExplanationDetailKeys, DetailApprovalRequired) {
		t.Fatalf("selfmonitor.ExplanationDetailKeys = %v, want it to hold %q",
			selfmonitor.ExplanationDetailKeys, DetailApprovalRequired)
	}
}

// A finding that waits for approval, with a reason naming pg_sage, is
// persisted: the e2e LLM default path lost every
// idle_in_transaction_session_timeout proposal (advisors-07) this way.
func TestApprovalRequiredFindingNamingPgSageIsAdvice(t *testing.T) {
	f := Finding{Category: "connection_tuning", ObjectType: "configuration",
		ObjectIdentifier: "instance", Title: "connection_tuning recommendation for instance",
		Detail: map[string]any{
			DetailApprovalRequired: "idle_in_transaction_session_timeout is changed only " +
				"with an operator's approval: it ends sessions, which pg_sage cannot verify",
			"rationale": "bound idle-in-transaction sessions",
		},
		RecommendedSQL: `ALTER DATABASE "app" SET idle_in_transaction_session_timeout = '300s'`,
		RollbackSQL:    `ALTER DATABASE "app" RESET idle_in_transaction_session_timeout`,
	}
	if excludedFromAdvice(f) {
		t.Fatal("an approval-required finding was dropped as pg_sage's own")
	}
	f.Detail["query"] = "SELECT * FROM sage.findings"
	if !excludedFromAdvice(f) {
		t.Fatal("a finding about a sage-schema statement was kept as advice")
	}
}
