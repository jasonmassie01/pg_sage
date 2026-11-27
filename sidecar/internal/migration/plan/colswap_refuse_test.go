package plan

import (
	"context"
	"strings"
	"testing"
)

// refuse asserts the planner refuses facts/req with a reason naming want.
func refuse(t *testing.T, facts SwapFacts, req SwapRequest, want string) {
	t.Helper()
	p, err := PlanColumnSwap(context.Background(), facts, req)
	if err == nil {
		t.Fatalf("planned %v, want a refusal naming %q", stepNames(p), want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to name %q", err, want)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("refusal still returned steps %v", stepNames(p))
	}
}

// Invalid input: a table without a primary key has no key to batch by.
func TestColumnSwapRefusesTableWithoutPrimaryKey(t *testing.T) {
	facts := textFacts()
	facts.PrimaryKey = nil
	refuse(t, facts, SwapRequest{Schema: "public", Table: "events", Column: "ref",
		TargetType: "bigint"}, "no primary key")
}

func TestColumnSwapRefusesCompositePrimaryKey(t *testing.T) {
	facts := textFacts()
	facts.PrimaryKey.Columns = []string{"id", "tenant"}
	refuse(t, facts, SwapRequest{Schema: "public", Table: "events", Column: "ref",
		TargetType: "bigint"}, "single-column primary key")
}

// Boundary: a foreign key from a partitioned table is out of scope; so is
// one from a partition (a clone of the parent's key).
func TestColumnSwapRefusesPartitionedReferencingFK(t *testing.T) {
	for _, mutate := range []func(*SwapForeignKey){
		func(fk *SwapForeignKey) { fk.ChildPartitioned = true },
		func(fk *SwapForeignKey) { fk.ChildIsPartition = true },
	} {
		facts := pkFacts()
		mutate(&facts.Referencing[0])
		refuse(t, facts, intReq(), "out of scope")
	}
}

func TestColumnSwapRefusesPartitionedOrInheritedTarget(t *testing.T) {
	for name, mutate := range map[string]func(*SwapFacts){
		"partitioned": func(f *SwapFacts) { f.RelKind = "p" },
		"partition":   func(f *SwapFacts) { f.IsPartition = true },
		"inheritance": func(f *SwapFacts) { f.HasInheritance = true },
		"view":        func(f *SwapFacts) { f.RelKind = "v" },
	} {
		facts := pkFacts()
		mutate(&facts)
		refuse(t, facts, intReq(), name)
	}
}

func TestColumnSwapRefusesColumnShapes(t *testing.T) {
	cases := map[string]func(*SwapFacts){
		"identity":  func(f *SwapFacts) { f.Column.Identity = true },
		"generated": func(f *SwapFacts) { f.Column.Generated = true },
		"depends on": func(f *SwapFacts) {
			f.Dependents = []string{"index orders_id_brin"}
		},
		"foreign key on the column": func(f *SwapFacts) {
			f.Outgoing = []string{"orders_customer_fk"}
		},
		"default": func(f *SwapFacts) {
			f.Column.Default, f.Column.DefaultIsSeq = "0", false
		},
		"multi-column foreign key": func(f *SwapFacts) { f.Referencing[0].Columns = 2 },
		"SET NULL column list":     func(f *SwapFacts) { f.Referencing[0].SetColumns = true },
	}
	for want, mutate := range cases {
		facts := pkFacts()
		mutate(&facts)
		refuse(t, facts, intReq(), want)
	}
}

// A sequence wider than the new column would hand out values it cannot
// hold.
func TestColumnSwapRefusesNarrowingSequence(t *testing.T) {
	facts := pkFacts()
	facts.Column.SequenceType = "bigint"
	req := intReq()
	req.TargetType = "integer"
	facts.Column.Type = "smallint"
	facts.PrimaryKey.ColumnType = "smallint"
	facts.Referencing[0].ColumnType = "smallint"
	refuse(t, facts, req, "sequence")
}

// A referencing column must stay comparable with the new key: within
// the integer family only.
func TestColumnSwapRefusesForeignKeyAcrossTypeFamilies(t *testing.T) {
	facts := pkFacts()
	facts.Referencing[0].ColumnType = "text"
	refuse(t, facts, intReq(), "referencing column")
}

func TestColumnSwapRefusesBadRequest(t *testing.T) {
	cases := map[string]SwapRequest{
		"target type":     {Schema: "public", Table: "orders", Column: "id", TargetType: "money"},
		"empty":           {Schema: "public", Table: "orders", Column: "", TargetType: "bigint"},
		"already":         {Schema: "public", Table: "orders", Column: "id", TargetType: "integer"},
		"request targets": {Schema: "public", Table: "other", Column: "id", TargetType: "bigint"},
	}
	for want, req := range cases {
		refuse(t, pkFacts(), req, want)
	}
	facts := pkFacts()
	facts.Column.Name = "other"
	refuse(t, facts, intReq(), "request targets")
}

// Boundary: a derived name of 63 bytes is kept; at 64 PostgreSQL would
// truncate it, so the plan is refused.
func TestColumnSwapDerivedNameLength(t *testing.T) {
	// "<table>_<column>_swap_sync" is the longest derived name.
	col := "c"
	fits := strings.Repeat("t", maxIdentBytes-len("_c_swap_sync"))
	facts := textFacts()
	facts.Table, facts.Column.Name = fits, col
	mustSwap(t, facts, SwapRequest{Schema: "public", Table: fits, Column: col,
		TargetType: "bigint"})
	long := fits + "t"
	facts.Table = long
	refuse(t, facts, SwapRequest{Schema: "public", Table: long, Column: col,
		TargetType: "bigint"}, "truncates")
}

// The sync function body is dollar-quoted with $swap$: a name holding the
// tag could end the body early, so it is refused.
func TestColumnSwapRefusesDollarTagInName(t *testing.T) {
	facts := textFacts()
	facts.Column.Name = "a$swap$b"
	refuse(t, facts, SwapRequest{Schema: "public", Table: "events", Column: "a$swap$b",
		TargetType: "bigint"}, "$swap$")
}

// Zero facts: nothing was read, so nothing can be planned.
func TestColumnSwapRefusesZeroFacts(t *testing.T) {
	refuse(t, SwapFacts{}, intReq(), "request targets")
}
