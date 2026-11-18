package plan

import (
	"context"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// A candidate whose evidence names a table and one of its indexes reaches
// the same statement twice. Each statement counts once: toward the calls
// a verdict needs and in its targets. No concurrent test: Build reads no
// shared state (TestBuild_ParallelDatabasesShareNoStepsAndRerunsHashTheSame).

// statusIndex is a generator index on public.orders(status) citing the
// table and the orders_pkey_dup index (whose key is status).
func statusIndex() candidate.Candidate {
	c := candidate.Candidate{Generator: "indexes", Dimension: "schema", Kind: "missing",
		Target: "public.orders", ActionType: "create_index_concurrently",
		ForwardSQL: "CREATE INDEX CONCURRENTLY sage_orders_status_idx ON " +
			"public.orders (status)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.sage_orders_status_idx",
		EvidenceIDs: []string{"schema/table/public.orders",
			"index/idx/public.orders_pkey_dup"},
		Reversible: true, Approval: candidate.ApprovalAutomatic, Reason: "status lookups",
		Prediction: candidate.Prediction{Metric: "blocks_per_call", Direction: "down",
			SizePct: ptr(20.0), Tier: "plan_only", Source: "hypopg"}}
	c.ID = candidate.NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL)
	return c
}

func TestBuild_AStatementReachedTwiceCountsOnce(t *testing.T) {
	in := baseInput(t, "shop")
	spec := fixtureSpec("shop")
	// 15 + 10 calls on public.orders: under the 30 a verdict needs, unless
	// statement 12 (status) is counted for the table and again for the index.
	spec.Workload.Families[0].QueryIDs[0].Calls = 15
	spec.Workload.Families[0].QueryIDs[1].Calls = 10
	in.Pack = mustPack(t, spec)
	gen := statusIndex()
	in.Candidates, in.Report.Ranked, in.Report.Hypotheses = []candidate.Candidate{gen},
		kept(gen), nil
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != nil {
		t.Fatalf("plan %+v: 25 calls in the window were counted as enough", res.Plan.Steps)
	}
	if a := adviceFor(t, res, "public.orders"); a.Reason != ReasonNotMeasurable {
		t.Fatalf("advice %+v, want not_measurable", a)
	}

	spec.Workload.Families[0].QueryIDs[1].Calls = 20
	in.Pack = mustPack(t, spec)
	res, err = Build(context.Background(), in)
	if err != nil || res.Plan == nil || len(res.Plan.Steps) != 1 {
		t.Fatalf("with 35 calls: %+v %v, want one step", res.Plan, err)
	}
	if got := res.Plan.Steps[0].TargetQueryIDs; !slices.Equal(got, []int64{11, 12}) {
		t.Fatalf("targets %v, want [11 12] once each", got)
	}
}
