package agentguard

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
)

// A role contract run by a leader job carries the earlier approval it runs
// under, and says it is scheduled, in the gate's evidence; a person's
// request carries no scheduled fields.
func TestPolicyRequest_ScheduledRunIsInTheEvidence(t *testing.T) {
	req := RoleRequest{PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa",
		Cluster:  Cluster{Key: "k"},
		Approval: Approval{ApprovedBy: 7, ApprovalID: 9},
		Scheduled: &ScheduledRun{Job: "retire_grace", OriginalApprovedBy: 7,
			OriginalActionID: 41, OriginalApprovalID: 9}}
	got, err := policyRequest(executor.ActionTypeGuardRoleRetire, req)
	if err != nil {
		t.Fatalf("policyRequest: %v", err)
	}
	if got.Evidence["scheduled"] != "retire_grace" {
		t.Fatalf("evidence scheduled = %v", got.Evidence["scheduled"])
	}
	orig, ok := got.Evidence["original_approval"].(ScheduledRun)
	if !ok || orig.OriginalActionID != 41 || orig.OriginalApprovedBy != 7 {
		t.Fatalf("evidence original_approval = %#v", got.Evidence["original_approval"])
	}
	if got.Evidence["approved_by"] != 7 {
		t.Fatalf("approved_by = %v", got.Evidence["approved_by"])
	}
	req.Scheduled = nil
	got, err = policyRequest(executor.ActionTypeGuardRoleRetire, req)
	if err != nil {
		t.Fatalf("policyRequest: %v", err)
	}
	if _, has := got.Evidence["scheduled"]; has {
		t.Fatal("a person's request must not read as scheduled")
	}
	if _, has := got.Evidence["original_approval"]; has {
		t.Fatal("a person's request has no original approval")
	}
}

func TestScheduledRun_AuditOnNilKeepsTheMap(t *testing.T) {
	var s *ScheduledRun
	m := s.audit(map[string]any{"retired": true})
	if len(m) != 1 || m["retired"] != true {
		t.Fatalf("audit on nil = %v", m)
	}
}
