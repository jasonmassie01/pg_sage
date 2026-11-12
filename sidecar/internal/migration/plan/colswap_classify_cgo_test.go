//go:build cgo

package plan

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
)

// CHECK-02: with the parse-tree classifier every generated DDL statement
// is known, and any rule it matches states the step's own lock. The
// backfill UPDATE is DML, which the DDL classifier leaves unrecognized
// by design; the planner states its ROW EXCLUSIVE.
func TestColumnSwapEveryStatementClassifiesKnown(t *testing.T) {
	for _, facts := range []SwapFacts{pkFacts(), textFacts()} {
		req := SwapRequest{Schema: facts.Schema, Table: facts.Table,
			Column: facts.Column.Name, TargetType: "bigint"}
		p := mustSwap(t, facts, req)
		if p.Degraded {
			t.Fatal("cgo build reports a degraded classification")
		}
		for _, s := range p.Steps {
			if s.Known == s.DML {
				t.Errorf("%s: known %v, DML %v: %q", s.Name, s.Known, s.DML, s.SQL)
			}
		}
		res, err := migration.ClassifyStatements(context.Background(), nil, p.Script(),
			facts.ServerVersion)
		if err != nil || res.Unrecognized() != 1 || res.Untokenizable {
			t.Fatalf("%s script: unrecognized %d (want the backfill only) "+
				"untokenizable %v err %v", facts.Table, res.Unrecognized(), res.Untokenizable, err)
		}
		for _, c := range res.All() {
			if c.RuleID == "ddl_missing_lock_timeout" {
				t.Fatalf("%s script is missing lock_timeout before %q", facts.Table, c.Statement)
			}
		}
	}
}
