package executor

import "testing"

func TestAnalyzeTableContractIsExecutableAndVerified(t *testing.T) {
	c := AnalyzeTableContract()

	if c.ActionType != "analyze_table" {
		t.Fatalf("ActionType = %q", c.ActionType)
	}
	if c.BaseRiskTier != "safe" {
		t.Fatalf("BaseRiskTier = %q", c.BaseRiskTier)
	}
	if len(c.Prechecks) == 0 {
		t.Fatalf("expected prechecks")
	}
	if len(c.PostChecks) == 0 {
		t.Fatalf("expected post-checks")
	}
	if c.RollbackClass != "no_rollback_needed" {
		t.Fatalf("RollbackClass = %q", c.RollbackClass)
	}
}

func TestActionContractRequiresPostChecks(t *testing.T) {
	c := ActionContract{ActionType: "bad_action", BaseRiskTier: "safe"}

	if err := c.Validate(); err == nil {
		t.Fatalf("expected validation error")
	}
}
