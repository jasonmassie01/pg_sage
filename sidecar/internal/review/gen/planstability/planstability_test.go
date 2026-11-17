package planstability

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Happy path (the Clerk shape): one set_column_statistics candidate,
// citing the null_frac and the query, then the ANALYZE that builds it.
func TestGenerate_ClerkShapeGivesOneStatisticsStep(t *testing.T) {
	src := source(map[string]TableFacts{"public.users": clerkUsers()})
	cands, cov := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))

	steps := byAction(cands, executor.ActionTypeSetColumnStatistics)
	if len(steps) != 1 {
		t.Fatalf("set_column_statistics candidates = %d (%+v), want 1", len(steps), cands)
	}
	c := steps[0]
	wantSQL := "ALTER TABLE public.users ALTER COLUMN deleted_at SET STATISTICS 1000"
	if c.ForwardSQL != wantSQL || c.Kind != KindNullCliff || c.Target != "public.users.deleted_at" {
		t.Fatalf("candidate = %s %s %q, want %s on public.users.deleted_at: %q", c.Kind,
			c.Target, c.ForwardSQL, KindNullCliff, wantSQL)
	}
	if want := "ALTER TABLE public.users ALTER COLUMN deleted_at SET STATISTICS -1"; c.
		RollbackSQL != want || !c.Reversible {
		t.Fatalf("rollback = %q reversible=%v, want %q", c.RollbackSQL, c.Reversible, want)
	}
	for _, id := range []string{queryID("101"), "stats/column/public.users.deleted_at"} {
		if !slices.Contains(c.EvidenceIDs, id) {
			t.Errorf("evidence %v does not cite %s", c.EvidenceIDs, id)
		}
	}
	if q := c.Prediction.Quantities; q["null_frac"] != 0.999996 || q["target_from"] != 100 ||
		q["target_to"] != 1000 {
		t.Errorf("quantities = %v, want null_frac 0.999996, 100 -> 1000", q)
	}
	if c.Dimension != Dimension || c.Generator != Name || c.ID == "" {
		t.Errorf("candidate identity = %s/%s/%q", c.Generator, c.Dimension, c.ID)
	}
	an := byAction(cands, "analyze_table")
	if len(an) != 1 || an[0].ForwardSQL != "ANALYZE public.users (deleted_at)" {
		t.Fatalf("analyze candidates = %+v, want ANALYZE public.users (deleted_at)", an)
	}
	if cov.Status != candidate.CoverageComplete {
		t.Errorf("coverage = %+v, want complete", cov)
	}
}

// CHECK-19: the 50%-NULL control column on the same table is not flagged.
func TestGenerate_ControlColumnNotFlagged(t *testing.T) {
	q := clerkQuery()
	q.text = "SELECT id FROM public.users WHERE status = $1 AND deleted_at IS NOT NULL"
	src := source(map[string]TableFacts{"public.users": clerkUsers()})
	cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
	for _, c := range cands {
		if strings.Contains(c.Target, "status") {
			t.Fatalf("the 50%%-NULL control column was flagged: %+v", c)
		}
	}
	if len(byAction(cands, executor.ActionTypeSetColumnStatistics)) != 1 {
		t.Fatalf("candidates = %+v, want only deleted_at's step", cands)
	}
}

func TestGenerate_NullFracBoundary(t *testing.T) {
	for _, tc := range []struct {
		nullFrac float64
		flagged  bool
	}{{0.9989, false}, {0.999, true}, {1, true}} {
		users := clerkUsers()
		users.Columns["deleted_at"] = col(tc.nullFrac, 4)
		src := source(map[string]TableFacts{"public.users": users})
		cands, _ := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))
		got := len(byAction(cands, executor.ActionTypeSetColumnStatistics)) == 1
		if got != tc.flagged {
			t.Errorf("null_frac %v flagged=%v, want %v", tc.nullFrac, got, tc.flagged)
		}
	}
}

// A range predicate on a NULL cliff is not the tiny-non-null rule.
func TestGenerate_NullCliffNeedsEqualityOrIsNotNull(t *testing.T) {
	q := clerkQuery()
	q.text = "SELECT id FROM public.users WHERE deleted_at > $1"
	src := source(map[string]TableFacts{"public.users": clerkUsers()})
	cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
	if len(cands) != 0 {
		t.Fatalf("range predicate on a NULL cliff gave %+v, want nothing", cands)
	}
}

func TestGenerate_CorrelationBoundary(t *testing.T) {
	for _, tc := range []struct {
		corr    float64
		mod     int64
		flagged bool
	}{{0.95, 200_000, false}, {0.9501, 200_000, true}, {0.9501, 100_000, false},
		{0.9501, 100_001, true}, {-0.99, 200_000, false}} {
		users := clerkUsers()
		c := col(0, -1)
		c.Correlation = f64(tc.corr)
		users.Columns["created_at"] = c
		users.ModSinceAnalyze = tc.mod
		q := clerkQuery()
		q.text = "SELECT id FROM public.users WHERE created_at >= $1"
		src := source(map[string]TableFacts{"public.users": users})
		cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
		steps := byAction(cands, executor.ActionTypeSetColumnStatistics)
		if got := len(steps) == 1 && steps[0].Kind == KindTimeOrdered; got != tc.flagged {
			t.Errorf("correlation %v mod %d flagged=%v, want %v (%+v)", tc.corr, tc.mod, got,
				tc.flagged, cands)
		}
	}
}

func TestGenerate_SkewBeyondTheSample(t *testing.T) {
	for _, tc := range []struct {
		top, nd float64
		flagged bool
	}{{0.51, 5000, true}, {0.5, 5000, false}, {0.51, 1000, false}, {0.51, -0.5, true},
		{0.51, -0.0005, false}} {
		users := clerkUsers()
		c := col(0, tc.nd)
		c.TopFreq = f64(tc.top)
		users.Columns["region"] = c
		q := clerkQuery()
		q.text = "SELECT id FROM public.users u WHERE u.region = $1"
		src := source(map[string]TableFacts{"public.users": users})
		cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
		steps := byAction(cands, executor.ActionTypeSetColumnStatistics)
		if got := len(steps) == 1 && steps[0].Kind == KindSkew; got != tc.flagged {
			t.Errorf("top %v n_distinct %v flagged=%v, want %v", tc.top, tc.nd, got, tc.flagged)
		}
	}
}

// The target is 10x the current one, capped at 10000; a column already
// at the cap gets advice, never a step that changes nothing.
func TestGenerate_TargetComputation(t *testing.T) {
	for _, tc := range []struct {
		current *int16
		deflt   int
		want    string
	}{{nil, 100, "1000"}, {i16(-1), 500, "5000"}, {i16(200), 100, "2000"},
		{i16(2000), 100, "10000"}, {i16(10000), 100, ""}} {
		users := clerkUsers()
		c := users.Columns["deleted_at"]
		c.StatTarget = tc.current
		users.Columns["deleted_at"] = c
		src := source(map[string]TableFacts{"public.users": users})
		src.facts.DefaultTarget = tc.deflt
		cands, _ := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))
		steps := byAction(cands, executor.ActionTypeSetColumnStatistics)
		if tc.want == "" {
			advice := byAction(cands, candidate.ActionAdvice)
			if len(steps) != 0 || len(advice) != 1 || !strings.Contains(advice[0].Reason,
				"10000") {
				t.Errorf("at the cap: %+v, want one advice naming the 10000 maximum", cands)
			}
			continue
		}
		if len(steps) != 1 || !strings.HasSuffix(steps[0].ForwardSQL, " "+tc.want) {
			t.Errorf("current %v default %d: %+v, want SET STATISTICS %s", tc.current, tc.deflt,
				steps, tc.want)
		}
	}
}

// Without table ownership the step is an operator step with the exact SQL.
func TestGenerate_NotOwnedIsOperatorStep(t *testing.T) {
	users := clerkUsers()
	users.Owned = false
	src := source(map[string]TableFacts{"public.users": users})
	cands, _ := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))
	ops := byAction(cands, candidate.ActionOperator)
	if len(ops) != 1 || len(byAction(cands, executor.ActionTypeSetColumnStatistics)) != 0 {
		t.Fatalf("candidates = %+v, want one operator step and no executor step", cands)
	}
	op := ops[0].Operator
	if op.Command != "ALTER TABLE public.users ALTER COLUMN deleted_at SET STATISTICS 1000; "+
		"ANALYZE public.users (deleted_at)" ||
		op.Revert != "ALTER TABLE public.users ALTER COLUMN deleted_at SET STATISTICS -1" ||
		op.Value != "1000" || ops[0].Approval != candidate.ApprovalRequired {
		t.Fatalf("operator = %+v approval %s", op, ops[0].Approval)
	}
}

// Invalid input: a column name with a quote or a semicolon is quoted. The
// executor's multi-statement guard refuses any ';' outside a literal, so
// the semicolon name is an operator step carrying the quoted SQL.
func TestGenerate_OddColumnNameIsQuoted(t *testing.T) {
	users := clerkUsers()
	users.Columns[`del"eted at`] = col(0.9999, 4)
	users.Columns[`del"eted;at`] = col(0.9999, 4)
	q := clerkQuery()
	q.text = `SELECT id FROM public.users WHERE "del""eted at" IS NOT NULL AND ` +
		`"del""eted;at" IS NOT NULL`
	src := source(map[string]TableFacts{"public.users": users})
	cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
	steps := byAction(cands, executor.ActionTypeSetColumnStatistics)
	want := `ALTER TABLE public.users ALTER COLUMN "del""eted at" SET STATISTICS 1000`
	if len(steps) != 1 || steps[0].ForwardSQL != want {
		t.Fatalf("steps = %+v, want %q", steps, want)
	}
	ops := byAction(cands, candidate.ActionOperator)
	want = `ALTER TABLE public.users ALTER COLUMN "del""eted;at" SET STATISTICS 1000; ` +
		`ANALYZE public.users ("del""eted;at")`
	if len(ops) != 1 || ops[0].Operator.Command != want {
		t.Fatalf("operator steps = %+v, want command %q", ops, want)
	}
}

func TestGenerate_NoTopQueriesNoCandidates(t *testing.T) {
	src := source(map[string]TableFacts{"public.users": clerkUsers()})
	cands, cov := generate(t, src, packOf(t, nil, nil, nil))
	if len(cands) != 0 || len(src.calls) != 0 {
		t.Fatalf("no top queries: %+v (source calls %v), want nothing read", cands, src.calls)
	}
	if cov.Status != candidate.CoverageComplete {
		t.Fatalf("coverage = %+v", cov)
	}
}

func TestGenerate_NeverAnalyzedIsUnknown(t *testing.T) {
	src := source(map[string]TableFacts{"public.users": {Status: StatusNeverAnalyzed,
		RelTuples: -1, Owned: true, Columns: map[string]ColumnFacts{"deleted_at": {}}}})
	cands, cov := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))
	if len(cands) != 0 {
		t.Fatalf("never analyzed gave %+v, want no candidate", cands)
	}
	assertUnknown(t, cov, "public.users", "never analyzed")
}

// Error propagation: no SELECT on the table gives unknown with the GRANT.
func TestGenerate_PermissionDeniedGivesGrant(t *testing.T) {
	src := source(map[string]TableFacts{"public.users": {Status: StatusNoPrivilege}})
	cands, cov := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))
	if len(cands) != 0 {
		t.Fatalf("no privilege gave %+v", cands)
	}
	assertUnknown(t, cov, "public.users", "GRANT SELECT ON TABLE public.users TO sage_agent")
}

func TestGenerate_MissingTableIsUnknown(t *testing.T) {
	src := source(map[string]TableFacts{})
	_, cov := generate(t, src, packOf(t, []query{clerkQuery()}, clerkStats(), nil))
	assertUnknown(t, cov, "public.users", "not found")
}

func assertUnknown(t *testing.T, cov candidate.Coverage, table, want string) {
	t.Helper()
	if cov.Status != candidate.CoveragePartial || !slices.Contains(cov.Missing,
		"pg_stats:"+table) {
		t.Fatalf("coverage = %+v, want partial with pg_stats:%s missing", cov, table)
	}
	for _, n := range cov.Notes {
		if strings.Contains(n, "unknown") && strings.Contains(n, want) {
			return
		}
	}
	t.Fatalf("notes %q do not say unknown with %q", cov.Notes, want)
}

func TestGenerate_SourceErrorPropagates(t *testing.T) {
	src := source(nil)
	src.err = errCatalog
	_, _, err := New(src).Generate(context.Background(),
		packOf(t, []query{clerkQuery()}, clerkStats(), nil), env)
	if !errors.Is(err, errCatalog) || !strings.Contains(err.Error(), "db1") ||
		!strings.Contains(err.Error(), "public.users") {
		t.Fatalf("err = %v, want the catalog error naming db1 and public.users", err)
	}
}

func TestGenerate_UnparseableTextSkippedWithReason(t *testing.T) {
	q := clerkQuery()
	q.text = "SELECT id FROM public.users WHERE deleted_at = 'unterminated"
	src := source(map[string]TableFacts{"public.users": clerkUsers()})
	cands, cov := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
	if len(cands) != 0 {
		t.Fatalf("unparseable text gave %+v", cands)
	}
	found := false
	for _, n := range cov.Notes {
		found = found || strings.Contains(n, "101") && strings.Contains(n, "unterminated")
	}
	if !found {
		t.Fatalf("notes %q do not give the skip reason for query 101", cov.Notes)
	}
}

func TestGenerate_InvalidArguments(t *testing.T) {
	g := New(source(nil))
	if _, _, err := g.Generate(context.Background(), nil, env); err == nil {
		t.Fatal("nil pack accepted")
	}
	p := packOf(t, nil, nil, nil)
	if _, _, err := g.Generate(context.Background(), p, candidate.Env{}); err == nil {
		t.Fatal("empty env accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := g.Generate(ctx, p, env); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx = %v", err)
	}
	if _, _, err := New(nil).Generate(context.Background(), p, env); err == nil {
		t.Fatal("a generator with no source accepted")
	}
}

// Generate keeps no state: many databases at once give the same answer.
func TestGenerate_ConcurrentCallsAgree(t *testing.T) {
	g := New(source(map[string]TableFacts{"public.users": clerkUsers()}))
	p := packOf(t, []query{clerkQuery()}, clerkStats(), nil)
	want, _, err := g.Generate(context.Background(), p, env)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := g.Generate(context.Background(), p, env)
			if err == nil && !reflect.DeepEqual(got, want) {
				err = errors.New("concurrent result differs")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// An operator step says why pg_sage cannot run it: ownership, or the
// executor refusing the statement; never ownership for a table it owns
// (review-02c review).
func TestGenerate_OperatorStepSaysWhy(t *testing.T) {
	users := clerkUsers()
	users.Columns[`del;at`] = col(0.9999, 4)
	q := clerkQuery()
	q.text = `SELECT id FROM public.users WHERE "del;at" IS NOT NULL`
	src := source(map[string]TableFacts{"public.users": users})
	cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
	ops := byAction(cands, candidate.ActionOperator)
	if len(ops) != 1 || strings.Contains(ops[0].Reason, "ownership") ||
		!strings.Contains(ops[0].Reason, "executor refuses") {
		t.Fatalf("owned table, refused name: operator steps %+v, want the refusal reason", ops)
	}
	users.Owned = false
	cands, _ = generate(t, source(map[string]TableFacts{"public.users": users}),
		packOf(t, []query{q}, clerkStats(), nil))
	ops = byAction(cands, candidate.ActionOperator)
	if len(ops) != 1 || !strings.Contains(ops[0].Reason, "needs table ownership") {
		t.Fatalf("not owned: operator steps %+v, want the ownership reason", ops)
	}
}

// A filtered column with no pg_stats row (a partitioned table keeps only
// inherited statistics) is unknown, noted once, never silently "no risk".
func TestGenerate_ColumnWithoutStatsIsNoted(t *testing.T) {
	users := clerkUsers()
	users.Columns["deleted_at"] = ColumnFacts{}
	second := clerkQuery()
	second.qid, second.execMs = "102", 50_000
	src := source(map[string]TableFacts{"public.users": users})
	cands, cov := generate(t, src, packOf(t, []query{clerkQuery(), second}, clerkStats(), nil))
	if len(cands) != 0 {
		t.Fatalf("no statistics gave %+v, want no candidate", cands)
	}
	n := 0
	for _, note := range cov.Notes {
		if strings.Contains(note, "public.users.deleted_at is unknown") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("notes %q: want exactly one unknown note for public.users.deleted_at",
			cov.Notes)
	}
}
