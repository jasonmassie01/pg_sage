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

func TestIncidentActionContractsValidate(t *testing.T) {
	for _, actionType := range []string{
		"diagnose_lock_blockers",
		"cancel_backend",
		"terminate_backend",
	} {
		c, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("%s contract missing", actionType)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("%s Validate: %v", actionType, err)
		}
		if len(c.Guardrails) == 0 {
			t.Fatalf("%s missing guardrails", actionType)
		}
		if len(c.AuditFields) == 0 {
			t.Fatalf("%s missing audit fields", actionType)
		}
	}
}

func TestIncidentActionContractRiskTiers(t *testing.T) {
	tests := map[string]string{
		"diagnose_lock_blockers": "safe",
		"cancel_backend":         "moderate",
		"terminate_backend":      "high",
	}
	for actionType, want := range tests {
		c, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("%s contract missing", actionType)
		}
		if c.BaseRiskTier != want {
			t.Fatalf("%s BaseRiskTier = %q, want %q",
				actionType, c.BaseRiskTier, want)
		}
	}
}

func TestContractForActionTypeIncidentActions(t *testing.T) {
	if _, ok := ContractForActionType("cancel_backend"); !ok {
		t.Fatal("cancel_backend contract missing")
	}
	if _, ok := ContractForActionType("rollback_prepared"); ok {
		t.Fatal("unexpected rollback_prepared contract")
	}
}
