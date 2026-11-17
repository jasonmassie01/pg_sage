package plan

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/gen/indexes"
	"github.com/pg-sage/sidecar/internal/review/report"
)

const statsSQL = "CREATE STATISTICS public.sage_stx_orders_region_country " +
	"(ndistinct, dependencies, mcv) ON region, country FROM public.orders"

func TestBuild_GeneratorAndModelGiveATwoStepPlan(t *testing.T) {
	in := baseInput(t, "shop")
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	p := res.Plan
	if p == nil || len(p.Steps) != 2 || len(res.Steps) != 2 {
		t.Fatalf("plan %+v with %d review steps, want two steps", p, len(res.Steps))
	}
	if p.Origin != changeplan.OriginModel || p.Model != testModel ||
		p.PackHash != in.Pack.ContentSHA256 || p.OriginRef != "review_run:41" {
		t.Errorf("plan origin %s model %q pack %q ref %q", p.Origin, p.Model, p.PackHash,
			p.OriginRef)
	}
	gen, mod := res.Steps[0], res.Steps[1]
	if gen.Origin != OriginGenerator || gen.OriginRef != "indexes" ||
		gen.CandidateID != customersIndex().ID || gen.ApprovalRequired {
		t.Errorf("generator step %+v", gen)
	}
	if mod.Origin != OriginModel || mod.OriginRef != testModel || !mod.ApprovalRequired ||
		!strings.HasPrefix(mod.ApprovalReason, changeplan.ReasonModelOrigin+":") {
		t.Errorf("model step %+v, want model origin and approval required", mod)
	}
	s := p.Steps[1]
	if s.ActionType != "create_statistics" || s.ProposedSQL != statsSQL ||
		s.RollbackSQL != "DROP STATISTICS IF EXISTS public.sage_stx_orders_region_country" {
		t.Errorf("model step SQL %q rollback %q", s.ProposedSQL, s.RollbackSQL)
	}
	if s.Metric != changeplan.MetricTimePerCall || fmt.Sprint(s.TargetQueryIDs) != "[11]" ||
		s.ExpectedSource != changeplan.SourceHypoPG || s.ExpectedPct == nil ||
		*s.ExpectedPct != 25 || s.ExpectedDirection != changeplan.DirectionDown {
		t.Errorf("model step measurement %+v", s)
	}
	if fmt.Sprint(s.EvidenceIDs) != "[schema/table/public.orders]" ||
		fmt.Sprint(mod.EvidenceIDs) != fmt.Sprint(s.EvidenceIDs) {
		t.Errorf("model step evidence %v / %v", s.EvidenceIDs, mod.EvidenceIDs)
	}
	g := p.Steps[0]
	if g.ProposedSQL != customersIndex().ForwardSQL || fmt.Sprint(g.TargetQueryIDs) != "[21]" ||
		g.Metric != changeplan.MetricBlocksPerCall || *g.ExpectedPct != 30 {
		t.Errorf("generator plan step %+v", g)
	}
	if mod.Prediction.Tier != TierHypoPG || mod.Measurement.DeadlineMinutes < 1 {
		t.Errorf("model step prediction %+v measurement %+v", mod.Prediction, mod.Measurement)
	}
	problems, err := changeplan.Validate(context.Background(), *p, acceptAll{pack: in.Pack})
	if err != nil || len(problems) != 0 {
		t.Fatalf("the plan does not validate: %v %+v", err, problems)
	}
	if res.Message != "" || res.Hypotheses != 1 || len(res.Advice) != 0 {
		t.Errorf("message %q hypotheses %d advice %+v", res.Message, res.Hypotheses, res.Advice)
	}
}

func TestBuild_KindsWithoutATemplateAreAdvice(t *testing.T) {
	for _, kind := range []string{"drop_table", "frobnicate", "drop_unused_index", ""} {
		in := baseInput(t, "shop")
		in.Report.Hypotheses = []report.Hypothesis{hyp(kind, "public.orders")}
		res, err := Build(context.Background(), in)
		if err != nil {
			t.Fatalf("%q: %v", kind, err)
		}
		a := adviceFor(t, res, "public.orders")
		if a.Reason != ReasonNoTemplate || a.Origin != OriginModel ||
			!strings.Contains(a.Detail, "no template") {
			t.Errorf("%q: advice %+v, want no_template", kind, a)
		}
		if strings.Contains(stepSQLs(res.Plan), "orders") {
			t.Errorf("%q: a step touches orders: %s", kind, stepSQLs(res.Plan))
		}
	}
}

func TestBuild_KindsWithoutAContractAreAdvice(t *testing.T) {
	for _, tc := range []struct{ kind, target string }{
		{"set_role_timeout", "app statement_timeout=30s"},
		{"set_role_connection_limit", "app connection_limit=20"},
		{"set_column_statistics", "public.orders(region) statistics=1000"},
	} {
		in := baseInput(t, "shop")
		in.Report.Hypotheses = []report.Hypothesis{hyp(tc.kind, tc.target)}
		res, err := Build(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if a := adviceFor(t, res, tc.target); a.Reason != ReasonNoContract ||
			!strings.Contains(a.Detail, tc.kind) {
			t.Errorf("%s: advice %+v, want no_contract naming the action type", tc.kind, a)
		}
	}
}

func TestBuild_UnknownObjectsAndBadTargetsAreAdvice(t *testing.T) {
	for _, tc := range []struct{ kind, target, reason string }{
		{"create_statistics", "public.orders(region, nosuch)", ReasonUnknownObject},
		{"create_statistics", "public.nosuch(a, b)", ReasonUnknownObject},
		{"create_index_concurrently", "shadow.orders(region)", ReasonUnknownObject},
		{"alter_database_guc", "no_such_setting=1", ReasonUnknownObject},
		{"create_statistics", "public.orders(region)", ReasonInvalidTarget},
		{"create_statistics", "public.orders(region, region)", ReasonInvalidTarget},
		{"create_index_concurrently", "public.orders(region; DROP TABLE x)",
			ReasonInvalidTarget},
		{"create_index_concurrently", "public.orders", ReasonInvalidTarget},
		{"create_index_concurrently", `public.orders("re""gion")`, ReasonInvalidTarget},
		{"alter_database_guc", "work_mem=lots", ReasonInvalidTarget},
		{"alter_database_guc", "shared_buffers=1GB", ReasonInvalidTarget},
	} {
		in := baseInput(t, "shop")
		in.Report.Hypotheses = []report.Hypothesis{hyp(tc.kind, tc.target)}
		res, err := Build(context.Background(), in)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.kind, tc.target, err)
		}
		if a := adviceFor(t, res, tc.target); a.Reason != tc.reason || a.Detail == "" {
			t.Errorf("%s %q: advice %+v, want %s", tc.kind, tc.target, a, tc.reason)
		}
		if res.Plan == nil || len(res.Plan.Steps) != 1 {
			t.Errorf("%s %q: plan %+v, want the generator step only", tc.kind, tc.target,
				res.Plan)
		}
	}
}

func TestBuild_ModelStepNeedsEvidenceThatResolves(t *testing.T) {
	in := baseInput(t, "shop")
	h := hyp("create_statistics", "public.orders(region, country)", "cand:abc", "nope/x")
	in.Report.Hypotheses = []report.Hypothesis{h}
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if a := adviceFor(t, res, h.Target); a.Reason != ReasonNoEvidence {
		t.Fatalf("advice %+v, want no_evidence", a)
	}
}

// The spec's injection CHECK: the statement text tells the model to drop
// orders_pkey_dup, citing E3. The model obeys; no template drops, and the
// generator's portfolio sees the index used, so no step names it.
func TestBuild_InjectedDropBecomesNoStep(t *testing.T) {
	in := baseInput(t, "shop")
	cands, _, err := indexes.New(nil).Generate(context.Background(), in.Pack,
		candidate.Env{Database: "shop", WindowDays: 30, MinImprovementPct: 10,
			MaxIndexesPerTable: 5})
	if err != nil {
		t.Fatalf("index generator: %v", err)
	}
	for _, c := range cands {
		if strings.Contains(c.Target, "orders_pkey_dup") && c.ActionType != candidate.ActionAdvice {
			t.Fatalf("the generator proposes %s on the used index", c.ActionType)
		}
	}
	in.Candidates = append(in.Candidates, cands...)
	in.Report.Hypotheses = []report.Hypothesis{hyp("drop_unused_index",
		"public.orders_pkey_dup", "workload/query/12")}
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stepSQLs(res.Plan), "orders_pkey_dup") {
		t.Fatalf("a step names the seeded object:\n%s", stepSQLs(res.Plan))
	}
	a := adviceFor(t, res, "public.orders_pkey_dup")
	if a.Reason != ReasonNoTemplate || !strings.Contains(a.Detail, "drop_unused_index") ||
		fmt.Sprint(a.EvidenceIDs) != "[workload/query/12]" {
		t.Fatalf("advice %+v, want the refusal naming drop_unused_index", a)
	}
}

func TestBuild_NothingProposedIsNoPlanAndNilPackIsAnError(t *testing.T) {
	in := baseInput(t, "shop")
	in.Candidates, in.Report.Ranked, in.Report.Hypotheses = nil, nil, nil
	res, err := Build(context.Background(), in)
	if err != nil || res.Plan != nil || res.Message != NoChangesProposed ||
		len(res.Steps) != 0 {
		t.Fatalf("empty review: %+v, %v; want no plan and %q", res, err, NoChangesProposed)
	}
	in.Pack = nil
	if _, err := Build(context.Background(), in); err == nil ||
		!strings.Contains(err.Error(), "pack") {
		t.Fatalf("nil pack: %v, want an error naming the pack", err)
	}
}

func TestBuild_RefusesInvalidInput(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"no run id":              func(in *Input) { in.RunID = 0 },
		"no database":            func(in *Input) { in.Database = " " },
		"negative minimum":       func(in *Input) { in.MinImprovementPct = -1 },
		"NaN minimum":            func(in *Input) { in.MinImprovementPct = math.NaN() },
		"pack of another dbname": func(in *Input) { in.Database = "other" },
	} {
		in := baseInput(t, "shop")
		mutate(&in)
		if _, err := Build(context.Background(), in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, baseInput(t, "shop")); err == nil {
		t.Error("a cancelled context built a plan")
	}
}

func TestBuild_LadderErrorsAndMissingTiersAreAdvice(t *testing.T) {
	in := baseInput(t, "shop")
	in.Ladder = &ladder{err: errLadder}
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	a := adviceFor(t, res, statsHyp().Target)
	if a.Reason != ReasonLadderError || !strings.Contains(a.Detail, errLadder.Error()) {
		t.Errorf("ladder error advice %+v", a)
	}
	if res.Plan == nil || len(res.Plan.Steps) != 1 || res.Plan.Origin != changeplan.OriginGenerator {
		t.Errorf("plan %+v, want the generator step alone with origin generator", res.Plan)
	}
	for name, l := range map[string]Ladder{"no tier": &ladder{by: map[string]float64{}},
		"nil ladder": nil} {
		in := baseInput(t, "shop")
		in.Ladder = l
		res, err := Build(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if a := adviceFor(t, res, statsHyp().Target); a.Reason != ReasonNoLadderTier {
			t.Errorf("%s: advice %+v, want no_ladder_tier", name, a)
		}
	}
}

func TestBuild_OverlappingHypothesesGiveOneStepAndANextReview(t *testing.T) {
	in := baseInput(t, "shop")
	in.Report.Hypotheses = []report.Hypothesis{statsHyp(),
		hyp("create_index_concurrently", "public.orders(region, country)")}
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan == nil || len(res.Plan.Steps) != 2 || res.Plan.Steps[1].ProposedSQL != statsSQL {
		t.Fatalf("plan %s, want the generator step and the statistics step", stepSQLs(res.Plan))
	}
	a := adviceFor(t, res, "public.orders(region, country)")
	if a.Reason != ReasonNextReview || a.Kind != "create_index_concurrently" ||
		!strings.Contains(a.Detail, "step 2") {
		t.Fatalf("advice %+v, want next_review naming step 2", a)
	}
}

func TestBuild_MinimumImprovementIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		pct  float64
		step bool
	}{{10, true}, {9.99, false}, {10.01, true}, {0, false}} {
		in := baseInput(t, "shop")
		in.Ladder = newLadder(tc.pct)
		res, err := Build(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		got := res.Plan != nil && len(res.Plan.Steps) == 2
		if got != tc.step {
			t.Errorf("prediction %v%%: step %v, want %v (%+v)", tc.pct, got, tc.step, res.Advice)
		}
		if !tc.step {
			if a := adviceFor(t, res, statsHyp().Target); a.Reason != ReasonBelowMinimum {
				t.Errorf("prediction %v%%: advice %+v, want below_minimum", tc.pct, a)
			}
		}
	}
}

func TestBuild_IndexCapSendsTheThirdIndexToTheNextReview(t *testing.T) {
	in := baseInput(t, "shop")
	in.Candidates, in.Report.Ranked = nil, nil
	in.Caps.MaxNewIndexesPerTable = 2
	spec := fixtureSpec("shop")
	in.Report.Hypotheses = []report.Hypothesis{
		hyp("create_index_concurrently", "public.orders(region)"),
		hyp("create_index_concurrently", "public.orders(status)"),
		hyp("create_index_concurrently", "public.orders(country)")}
	// Each index serves its own statement, so only the cap can stop one.
	in.Pack = mustPack(t, splitQueries(spec))
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan == nil || len(res.Plan.Steps) != 2 {
		t.Fatalf("plan %s, want two indexes", stepSQLs(res.Plan))
	}
	a := adviceFor(t, res, "public.orders(country)")
	if a.Reason != ReasonNextReview || !strings.Contains(a.Detail, "cap") {
		t.Fatalf("advice %+v, want next_review naming the cap", a)
	}
}

func TestBuild_DatabaseSettingRendersWithItsRollback(t *testing.T) {
	for _, tc := range []struct{ target, sql, rollback string }{
		{"work_mem=64MB", `ALTER DATABASE "shop" SET work_mem = '64MB'`,
			`ALTER DATABASE "shop" RESET work_mem`},
		{"random_page_cost=1.1", `ALTER DATABASE "shop" SET random_page_cost = '1.1'`,
			`ALTER DATABASE "shop" SET random_page_cost = '1.5'`},
	} {
		in := baseInput(t, "shop")
		in.Report.Hypotheses = []report.Hypothesis{hyp("alter_database_guc", tc.target,
			"settings/work_mem")}
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

func TestBuild_GeneratorAdviceAndUnmeasurableCandidatesAreAdvice(t *testing.T) {
	in := baseInput(t, "shop")
	adv := candidate.Candidate{Generator: "schemadebt", Kind: "partition",
		Target: "public.orders", ActionType: candidate.ActionAdvice,
		EvidenceIDs: []string{"schema/table/public.orders"}, Reason: "range partition it",
		Approval: candidate.ApprovalRequired}
	adv.ID = candidate.NewID(adv.Generator, adv.Kind, adv.Target, adv.ForwardSQL)
	in.Candidates = append(in.Candidates, adv)
	in.Report.Ranked = kept(customersIndex(), adv)
	in.Report.Hypotheses = nil
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	a := adviceFor(t, res, "public.orders")
	if a.Reason != ReasonAdviceOnly || a.Origin != OriginGenerator || a.OriginRef != "schemadebt" {
		t.Fatalf("advice %+v, want advice_only from schemadebt", a)
	}
	if res.Plan == nil || res.Plan.Origin != changeplan.OriginGenerator || res.Plan.Model != "" {
		t.Fatalf("plan %+v, want a generator plan with no model", res.Plan)
	}
}

func TestBuild_DroppedCandidatesAreNotPlanned(t *testing.T) {
	in := baseInput(t, "shop")
	in.Report.Ranked = nil
	in.Report.Hypotheses = nil
	res, err := Build(context.Background(), in)
	if err != nil || res.Plan != nil || res.Message != NoChangesProposed {
		t.Fatalf("a candidate the review did not keep was planned: %+v %v", res, err)
	}
}

func TestBuild_ModelAuthorityFollowsTheClassLevel(t *testing.T) {
	in := baseInput(t, "shop")
	in.ModelAuthority = true
	res, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[1].ApprovalRequired || res.Steps[1].ApprovalReason != "" {
		t.Fatalf("model step after G-M1: %+v, want its class level (automatic)", res.Steps[1])
	}
	in.Report.Hypotheses = []report.Hypothesis{hyp("alter_database_guc", "work_mem=64MB",
		"settings/work_mem")}
	in.Settings = changeplan.SettingContexts{"work_mem": "user"}
	res, err = Build(context.Background(), in)
	if err != nil || len(res.Steps) != 2 {
		t.Fatalf("%v %+v", err, res.Advice)
	}
	if res.Steps[1].ApprovalRequired {
		t.Fatalf("a user-context setting step after G-M1 needs approval: %+v", res.Steps[1])
	}
}

func TestBuild_ParallelDatabasesShareNoStepsAndRerunsHashTheSame(t *testing.T) {
	var wg sync.WaitGroup
	results := make([]Result, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := []string{"shop", "ledger"}[i%2]
			results[i], errs[i] = Build(context.Background(), baseInput(t, db))
		}(i)
	}
	wg.Wait()
	hashes := map[string]string{}
	for i, r := range results {
		if errs[i] != nil || r.Plan == nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		db := []string{"shop", "ledger"}[i%2]
		h := changeplan.PlanHash(*r.Plan)
		if prev, ok := hashes[db]; ok && prev != h {
			t.Fatalf("%s: two builds of one review hash %s and %s", db, prev, h)
		}
		hashes[db] = h
		if !strings.Contains(r.Plan.Goal, db) {
			t.Fatalf("run %d: goal %q does not name %s", i, r.Plan.Goal, db)
		}
	}
	if hashes["shop"] == hashes["ledger"] {
		t.Fatal("the two databases' plans have one hash")
	}
	results[0].Plan.Steps[0].ProposedSQL = "changed"
	if results[2].Plan.Steps[0].ProposedSQL == "changed" {
		t.Fatal("two builds share a step")
	}
}
