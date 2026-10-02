package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The repair contract (AI-SRE-SPEC §7.2) every Sage SRE action class
// carries: preconditions, scope from evidence ids, lock impact and
// timeouts, reversibility class, rollback trigger and inverse,
// post-conditions, blast-radius budget and a never-do list.

func TestCancelBackendRepairContractIsComplete(t *testing.T) {
	c := CancelBackendRepairContract()
	if err := c.Validate(); err != nil {
		t.Fatalf("cancel contract invalid: %v", err)
	}
	if c.ActionType != "cancel_backend" || c.BaseRiskTier != "moderate" ||
		c.RollbackClass != ReversibilityMitigationOnly ||
		c.Reversibility != ReversibilityMitigationOnly {
		t.Fatalf("cancel contract classes = %s/%s/%s/%s", c.ActionType, c.BaseRiskTier,
			c.RollbackClass, c.Reversibility)
	}
	if c.BlastRadius != (BlastRadiusBudget{MaxBackends: 1, MaxDatabases: 1,
		MaxActionsPerInvestigation: 1}) {
		t.Fatalf("blast radius = %+v, want one backend in one database", c.BlastRadius)
	}
	joined := strings.ToLower(strings.Join(append(append(append([]string{},
		c.Preconditions...), c.NeverDo...), c.PostConditions...), "\n"))
	for _, want := range []string{"5 s", "backend_start", "query hash",
		"pg_terminate_backend", "idle in transaction", "recovery predicate",
		"wait edges"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cancel contract never mentions %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(c.Scope), "evidence") ||
		!strings.Contains(strings.ToLower(c.ResidualRisk), "race") ||
		c.Inverse == "" || c.RollbackTrigger == "" || c.LockImpact == "" ||
		len(c.Timeouts) == 0 || c.Version == "" {
		t.Fatalf("cancel contract incomplete: %+v", c)
	}
}

func TestRepairContractValidateRejectsGaps(t *testing.T) {
	for name, mutate := range map[string]func(*RepairContract){
		"base contract":       func(c *RepairContract) { c.ActionType = "" },
		"no version":          func(c *RepairContract) { c.Version = "" },
		"no preconditions":    func(c *RepairContract) { c.Preconditions = nil },
		"no scope":            func(c *RepairContract) { c.Scope = " " },
		"no lock impact":      func(c *RepairContract) { c.LockImpact = "" },
		"no timeouts":         func(c *RepairContract) { c.Timeouts = nil },
		"unknown reversible":  func(c *RepairContract) { c.Reversibility = "maybe" },
		"no rollback trigger": func(c *RepairContract) { c.RollbackTrigger = "" },
		"no inverse":          func(c *RepairContract) { c.Inverse = "" },
		"no post-conditions":  func(c *RepairContract) { c.PostConditions = nil },
		"zero blast radius":   func(c *RepairContract) { c.BlastRadius = BlastRadiusBudget{} },
		"no never-do":         func(c *RepairContract) { c.NeverDo = nil },
		"rollback mismatch": func(c *RepairContract) {
			c.RollbackClass = "reversible"
		},
	} {
		c := CancelBackendRepairContract()
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrRepairContractInvalid) {
			t.Errorf("%s: Validate = %v, want ErrRepairContractInvalid", name, err)
		}
	}
}

// Cancellation does not undo committed work: it is mitigation only for
// every path that cancels a backend, and the gate knows the class.
func TestCancelBackendContractIsMitigationOnly(t *testing.T) {
	c, ok := ContractForActionType("cancel_backend")
	if !ok || c.RollbackClass != ReversibilityMitigationOnly {
		t.Fatalf("cancel_backend rollback class = %q", c.RollbackClass)
	}
	if got := policyContract(c).RollbackClass; got != policy.RollbackMitigationOnly {
		t.Fatalf("policy rollback class = %q", got)
	}
	if err := policy.ValidateContract(*policyContract(c)); err != nil {
		t.Fatalf("policy rejects the cancel contract: %v", err)
	}
	term, _ := ContractForActionType("terminate_backend")
	if term.RollbackClass != "not_reversible" {
		t.Fatalf("terminate rollback class = %q, want not_reversible", term.RollbackClass)
	}
}
