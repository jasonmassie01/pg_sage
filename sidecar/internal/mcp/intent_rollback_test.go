package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// D1: every intent contract declares its rollback class, so the refusal
// set's "unrollbackable" token sees a typed value instead of an empty one.
func TestIntentContractsDeclareRollbackClass(t *testing.T) {
	tests := []struct {
		tool, arguments string
		want            policy.RollbackClass
	}{
		{"optimize_query", `{"table":"public.orders"}`, policy.RollbackReversible},
		{"ensure_fk_indexes", `{"schema":"public"}`, policy.RollbackReversible},
		{"apply_migration", `{"table":"public.orders"}`, policy.RollbackForwardFixOnly},
		{"declare_table_contract", `{"table":"public.orders"}`, policy.RollbackReversible},
		{"register_consumer", `{"slot_name":"cdc"}`, policy.RollbackReversible},
		{"request_change", `{"kind":"apply_migration"}`, policy.RollbackForwardFixOnly},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			req, err := DeterministicIntentPlanner{}.Plan(
				context.Background(), tt.tool, json.RawMessage(tt.arguments))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if req.Contract.RollbackClass != tt.want {
				t.Fatalf("RollbackClass = %q, want %q", req.Contract.RollbackClass, tt.want)
			}
		})
	}
}

func TestProductionIntentContractsDeclareRollbackClass(t *testing.T) {
	if got := candidateIndexContract().RollbackClass; got != policy.RollbackReversible {
		t.Fatalf("candidate index RollbackClass = %q, want reversible", got)
	}
	migration := onlineMigrationContract()
	if migration.RollbackClass != policy.RollbackForwardFixOnly ||
		migration.RiskTier != policy.RiskModerate {
		t.Fatalf("online migration contract = %+v, want moderate forward_fix_only", migration)
	}
}
