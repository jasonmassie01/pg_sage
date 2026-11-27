//go:build cgo

package plan

import (
	"context"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
)

// No concurrent access or state tests: planning and classification are
// pure functions of the facts.

// swapStepRules is every column-swap step and the DDL rules the parse
// tree must find in it (none: a metadata-only form it proves safe).
var swapStepRules = map[string][]string{
	"set_lock_timeout": nil, "set_plan_cache_mode": nil, "add_column": nil,
	"create_sync_function": nil, "create_sync_trigger": {"ddl_create_trigger"},
	"add_not_null_proof": nil, "validate_not_null_proof": nil,
	"build_unique_index": nil, "begin": nil, "lock_tables": {"ddl_lock_table"},
	"drop_referencing_fk": nil, "drop_primary_key": nil,
	"drop_sync_trigger": {"ddl_drop_trigger"}, "drop_old_default": nil,
	"drop_old_not_null": nil, "rename_old_column": {"ddl_rename"},
	"rename_new_column": {"ddl_rename"}, "set_not_null": {"ddl_set_not_null"},
	"drop_not_null_proof": nil, "attach_primary_key": nil, "set_default": nil,
	"move_sequence": nil, "add_referencing_fk": nil, "commit": nil,
	"validate_referencing_fk": nil, "drop_sync_function": {"ddl_drop_function"},
	"drop_old_column": {"ddl_drop_column"},
}

// CHECK-02 (migration-09b) as a table: every step the planner emits
// classifies through the parse tree as its plan step claims. The step is
// known, its recorded rules are the table's, an independent
// classification finds the same rules, and none states a stronger lock
// than the step does. The backfill is DML: unrecognized by design.
func TestColumnSwapStepsClassifyAsTheyClaim(t *testing.T) {
	seen := map[string]bool{}
	for _, facts := range []SwapFacts{pkFacts(), textFacts()} {
		req := SwapRequest{Schema: facts.Schema, Table: facts.Table,
			Column: facts.Column.Name, TargetType: "bigint"}
		p := mustSwap(t, facts, req)
		if p.Degraded {
			t.Fatal("cgo build reports a degraded classification")
		}
		for _, s := range p.Steps {
			seen[s.Name] = true
			checkSwapStep(t, facts, s)
		}
	}
	for name := range swapStepRules {
		if !seen[name] {
			t.Errorf("table step %s is in no plan: the table is stale", name)
		}
	}
	if !seen["backfill"] {
		t.Error("no plan has a backfill step")
	}
}

func checkSwapStep(t *testing.T, facts SwapFacts, s SwapStep) {
	t.Helper()
	if s.DML {
		if s.Name != "backfill" || s.Known {
			t.Errorf("DML step %s known %v, want the unrecognized backfill", s.Name, s.Known)
		}
		return
	}
	want, ok := swapStepRules[s.Name]
	if !ok {
		t.Errorf("step %s is not in the table: %q", s.Name, s.SQL)
		return
	}
	if !s.Known || !slices.Equal(s.Rules, want) {
		t.Errorf("%s/%s: known %v rules %v, want known %v: %q", facts.Table, s.Name,
			s.Known, s.Rules, want, s.SQL)
	}
	res, err := migration.ClassifyStatements(context.Background(), nil, s.SQL,
		facts.ServerVersion)
	if err != nil || res.Degraded || res.Unrecognized() != 0 || len(res.Statements) != 1 {
		t.Fatalf("%s: %+v %v, want one known statement", s.Name, res, err)
	}
	var got []string
	for _, c := range res.All() {
		if c.RuleID == "ddl_missing_lock_timeout" {
			continue
		}
		got = append(got, c.RuleID)
		if lockRank[c.LockLevel] > lockRank[s.LockLevel] {
			t.Errorf("%s states %s; %s reports %s", s.Name, s.LockLevel, c.RuleID,
				c.LockLevel)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: parse tree finds %v, want %v", s.Name, got, want)
	}
}
