package tuning

import (
	"slices"
	"testing"
)

// indexes-05 review: the HypoPG what-if plans at most 25 statements, the
// recommendation's affected queryids first. The whatif_index tool must
// hand over the case's statements: otherwise, on a table with more than
// 25 statements, the tool's measurement can miss the case's own statement
// and record a rejection in rejection memory that later suppresses the
// targeted proposal.

func TestTools_WhatIfHandsTheCaseStatementsToTheWhatIf(t *testing.T) {
	h := newHarness(t)
	seen := recordAffected(h)
	tb := ordersToolbox(t, h)
	want := caseStatementIDs(tb.c)
	if len(want) == 0 {
		t.Fatal("fixture case has no statements")
	}
	out := call(t, tb, "whatif_index",
		`{"ddl":"CREATE INDEX w ON public.orders (customer_id)"}`)
	if out["verdict"] != "verified" {
		t.Fatalf("whatif = %v", out)
	}
	if got := seen(); !slices.Equal(got, want) {
		t.Fatalf("what-if affected queryids = %v, want the case's statements %v", got, want)
	}
}

func TestCaseStatementIDsNilAndDuplicates(t *testing.T) {
	if got := caseStatementIDs(Case{}); len(got) != 0 {
		t.Fatalf("a case without statements gave %v", got)
	}
	c := Case{Statements: []CaseStatement{{QueryID: 7}, {QueryID: 0}, {QueryID: 7},
		{QueryID: 9}}}
	if got := caseStatementIDs(c); !slices.Equal(got, []int64{7, 9}) {
		t.Fatalf("caseStatementIDs = %v, want [7 9]", got)
	}
}
