package plan

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// The templates' rendering: database settings with their exact rollback,
// identifier quoting and hashed identifiers. No concurrent access tests:
// rendering is a pure function of the hypothesis and the pack.

func TestBuild_DatabaseSettingRendersWithItsRollback(t *testing.T) {
	for _, tc := range []struct{ target, sql, rollback string }{
		{"work_mem=64MB", `ALTER DATABASE "shop" SET work_mem = '64MB'`,
			`ALTER DATABASE "shop" RESET work_mem`},
		{"random_page_cost=1.1", `ALTER DATABASE "shop" SET random_page_cost = '1.1'`,
			`ALTER DATABASE "shop" SET random_page_cost = '1.5'`},
	} {
		in := baseInput(t, "shop")
		in.Report.Hypotheses = []report.Hypothesis{hyp("alter_database_guc", tc.target,
			"setting/work_mem")}
		res, err := Build(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Plan == nil || len(res.Plan.Steps) != 2 {
			t.Fatalf("%s: plan %s advice %+v", tc.target, stepSQLs(res.Plan), res.Advice)
		}
		s := res.Plan.Steps[1]
		if s.ProposedSQL != tc.sql || s.RollbackSQL != tc.rollback ||
			s.Metric != changeplan.MetricReadback || s.Setting == "" {
			t.Errorf("%s: step %q / %q metric %s setting %q", tc.target, s.ProposedSQL,
				s.RollbackSQL, s.Metric, s.Setting)
		}
	}
}

func TestBuild_IndexTemplateQuotesIdentifiers(t *testing.T) {
	in := baseInput(t, "shop")
	in.Candidates, in.Report.Ranked = nil, nil
	in.Report.Hypotheses = []report.Hypothesis{hyp("create_index_concurrently",
		`public."Orders"("Region")`, "schema/table/public.Orders")}
	spec := fixtureSpec("shop")
	spec.Texts.Texts = append(spec.Texts.Texts, workpackText("31",
		`SELECT 1 FROM public."Orders" WHERE "Region" = $1`))
	spec.Workload.Families[0].QueryIDs = append(spec.Workload.Families[0].QueryIDs,
		workpackQuery("31"))
	in.Pack = mustPack(t, spec)
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan == nil || len(res.Plan.Steps) != 1 {
		t.Fatalf("plan %s advice %+v", stepSQLs(res.Plan), res.Advice)
	}
	s := res.Plan.Steps[0]
	want := `CREATE INDEX CONCURRENTLY sage_orders_region_idx ON public."Orders" ("Region")`
	if s.ProposedSQL != want ||
		s.RollbackSQL != "DROP INDEX CONCURRENTLY IF EXISTS public.sage_orders_region_idx" {
		t.Fatalf("SQL %q rollback %q, want %q", s.ProposedSQL, s.RollbackSQL, want)
	}
}

func TestBuild_HashedIdentifiersMapBackLocally(t *testing.T) {
	in := baseInput(t, "shop")
	in.Pseudonyms = map[string]string{"t_9f2c": "orders", "c_11aa": "region",
		"c_22bb": "country"}
	in.Report.Hypotheses = []report.Hypothesis{hyp("create_statistics",
		"public.t_9f2c(c_11aa, c_22bb)")}
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan == nil || len(res.Plan.Steps) != 2 || res.Plan.Steps[1].ProposedSQL != statsSQL {
		t.Fatalf("plan %s advice %+v, want the statistics step on the real names",
			stepSQLs(res.Plan), res.Advice)
	}
	in.Report.Hypotheses = []report.Hypothesis{hyp("create_statistics",
		"public.t_0000(c_11aa, c_22bb)")}
	res, err = Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if a := adviceFor(t, res, "public.t_0000(c_11aa, c_22bb)"); a.Reason != ReasonUnknownObject {
		t.Fatalf("an unmapped pseudonym gave %+v, want unknown_object", a)
	}
}
