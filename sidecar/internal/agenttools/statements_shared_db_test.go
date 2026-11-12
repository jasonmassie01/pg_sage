package agenttools

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// market-01: the workload explorer reads statement texts through the same
// presentation as top_queries, so both show the same text and leave out
// the same statements.
// No concurrent access tests: Statements only reads pg_stat_statements.

func TestStatementsMatchTopQueriesAndFlagPgSageStatements(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	w := newTopWorkload(f)
	tools := New(f.pool, Options{})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		w.run(f)
		workID, ok := f.statementID("status")
		selfID, okSelf := f.statementID("max(amount)")
		if !ok || !okSelf {
			return []string{"workload or pg_sage-tagged statement missing"}
		}
		top, err := tools.TopQueries(f.ctx, TopQueriesRequest{Limit: 50, OrderBy: "calls"})
		if err != nil {
			t.Fatalf("TopQueries: %v", err)
		}
		listed := findTop(top.Queries, w.orders, "status")
		if listed == nil {
			return []string{"workload missing from top_queries"}
		}
		got, err := tools.Statements(f.ctx, []int64{workID, selfID, 1})
		if err != nil {
			t.Fatalf("Statements: %v", err)
		}
		work, self := got[workID], got[selfID]
		if work.Query != listed.Query || work.Excluded || work.Tags["route"] != "/cart" {
			t.Fatalf("workload statement %+v, want top_queries' text %q, kept, with tags",
				work, listed.Query)
		}
		if !self.Excluded || self.Query == "" {
			t.Fatalf("pg_sage statement %+v, want its text flagged excluded", self)
		}
		if _, ok := got[1]; ok {
			t.Fatal("an untracked queryid has a statement, want it absent")
		}
		return nil
	})
}

func TestStatementsAndPlansWithoutPool(t *testing.T) {
	var tools *Tools
	if _, err := tools.Statements(t.Context(), []int64{1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Statements without a pool = %v, want ErrUnavailable", err)
	}
	if _, err := tools.Plans(t.Context(), []int64{1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Plans without a pool = %v, want ErrUnavailable", err)
	}
}
