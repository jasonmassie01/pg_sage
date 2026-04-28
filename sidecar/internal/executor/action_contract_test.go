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

func TestAlterTableContractIsForwardFixOnly(t *testing.T) {
	c, ok := ContractForActionType("alter_table")
	if !ok {
		t.Fatalf("alter_table contract missing")
	}
	if c.BaseRiskTier != "high" {
		t.Fatalf("BaseRiskTier = %q, want high", c.BaseRiskTier)
	}
	if c.RollbackClass != "forward_fix_only" {
		t.Fatalf("RollbackClass = %q, want forward_fix_only",
			c.RollbackClass)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
