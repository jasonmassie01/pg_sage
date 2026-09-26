package plan

import (
	"context"
	"strings"
	"testing"
)

// TestPlannerRejectsMultiClauseAlter is the G7-B24 regression: only
// the first matching clause was planned and the rest silently dropped.
func TestPlannerRejectsMultiClauseAlter(t *testing.T) {
	cases := []string{
		`ALTER TABLE public.users ALTER COLUMN a SET NOT NULL, ` +
			`ALTER COLUMN b SET NOT NULL`,
		`ALTER TABLE public.users ADD CONSTRAINT u UNIQUE (email), ` +
			`ADD COLUMN x int`,
		`ALTER TABLE public.users ALTER COLUMN a SET NOT NULL; DROP TABLE x`,
	}
	for _, sql := range cases {
		_, err := NewPlanner().Plan(context.Background(), Request{
			SQL: sql, Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
		})
		if err == nil || !strings.Contains(err.Error(), "single") {
			t.Fatalf("%q: err = %v, want single-clause rejection", sql, err)
		}
	}
}

// TestPlannerRejectsTableMismatch: the SQL's target table must be the
// table the caller described, or the plan would alter a different one.
func TestPlannerRejectsTableMismatch(t *testing.T) {
	_, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.orders ALTER COLUMN a SET NOT NULL`,
		Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
	})
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("err = %v, want table mismatch naming orders", err)
	}
	// Unqualified, case-folded names resolve against the request.
	_, err = NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE Users ALTER COLUMN a SET NOT NULL`,
		Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil {
		t.Fatalf("unqualified matching table rejected: %v", err)
	}
}

// TestPlannerAllowsCommaInsideUniqueColumns: a column-list comma is
// not a second clause.
func TestPlannerAllowsCommaInsideUniqueColumns(t *testing.T) {
	result, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.users ADD CONSTRAINT u UNIQUE (a, b)`,
		Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil || result.Classification != ClassificationAddUnique {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
