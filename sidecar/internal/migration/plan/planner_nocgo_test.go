//go:build !cgo

package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// migration-02b: without the parse tree the planner refuses every
// statement it would otherwise plan, naming the build it needs, and
// returns no plan. Statements validateStatement refuses still get that
// refusal first.
func TestPlannerRefusesWithoutTheParseTree(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`,
		`ALTER TABLE public.users ALTER COLUMN email SET NOT NULL`,
	} {
		result, err := NewPlanner().Plan(context.Background(),
			Request{SQL: sql, Cycle: 3, Table: usersTable()})
		if !errors.Is(err, sqlast.ErrUnavailable) {
			t.Fatalf("Plan(%q) error = %v, want sqlast.ErrUnavailable", sql, err)
		}
		if !strings.Contains(err.Error(),
			"online migration planning needs the parse-tree build") {
			t.Fatalf("Plan(%q) error = %q, want the build it needs", sql, err)
		}
		if result.Classification != "" || len(result.ExpandSteps) != 0 ||
			len(result.ContractSteps) != 0 {
			t.Fatalf("Plan(%q) refused but returned %#v", sql, result)
		}
	}
	_, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.users ALTER COLUMN a SET NOT NULL, ALTER COLUMN b SET NOT NULL`,
		Cycle: 1, Table: usersTable(),
	})
	if err == nil || errors.Is(err, sqlast.ErrUnavailable) ||
		!strings.Contains(err.Error(), "single") {
		t.Fatalf("multi-clause error = %v, want the single-clause refusal", err)
	}
}
