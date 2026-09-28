package runtime

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
)

// D1: an online migration step is forward-fix-only; the policy contract
// must say so for the refusal set to see it.
func TestMigrationActionDeclaresForwardFixOnly(t *testing.T) {
	action := migrationAction(Request{Table: plan.TableFacts{Schema: "public", Name: "t"}},
		plan.Step{SQL: "ALTER TABLE public.t ADD COLUMN c int"})
	if action.Contract.RollbackClass != policy.RollbackForwardFixOnly {
		t.Fatalf("RollbackClass = %q, want forward_fix_only", action.Contract.RollbackClass)
	}
}
