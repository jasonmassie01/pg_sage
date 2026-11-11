package tuner

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// tuning-11: parameterization advice has a producer. The detector reads the
// collector's latest snapshot; it adds no pg_stat_statements read.
//
// No concurrent access tests: the detector runs inside Tune under t.mu, and
// its only state (the cycle's emitted shapes) is reset there.

type fakeQuerySource struct{ snap *collector.Snapshot }

func (f fakeQuerySource) LatestSnapshot() *collector.Snapshot { return f.snap }

// listVariants is n statements of one shape whose IN lists have 1..n
// parameters, each its own queryid (pg_stat_statements before PG18).
func listVariants(table string, n int, firstID int64) []collector.QueryStats {
	out := make([]collector.QueryStats, 0, n)
	for i := 1; i <= n; i++ {
		params := make([]string, i)
		for p := range params {
			params[p] = fmt.Sprintf("$%d", p+1)
		}
		out = append(out, collector.QueryStats{
			QueryID: firstID + int64(i),
			Query: "SELECT id, total FROM " + table + " WHERE id IN (" +
				strings.Join(params, ", ") + ")",
			Calls: 10, TotalExecTime: 2.5, MeanExecTime: 0.25,
			TotalPlanTime: 1, MeanPlanTime: 0.1,
		})
	}
	return out
}

func paramTuner(cfg TunerConfig, queries []collector.QueryStats, pgNum int) *Tuner {
	tu := New(nil, cfg, nil, noopLogFn)
	tu.WithQuerySource(fakeQuerySource{snap: &collector.Snapshot{Queries: queries}}, pgNum)
	return tu
}

func paramFindings(fs []analyzer.Finding) []analyzer.Finding {
	var out []analyzer.Finding
	for _, f := range fs {
		if f.Category == "query_parameterization" {
			out = append(out, f)
		}
	}
	return out
}

var shapeIdent = regexp.MustCompile(`^shape:[0-9a-f]{16}$`)

// Boundary: 20 list variants of one shape emit one finding.
func TestParameterization_TwentyVariantsEmitOne(t *testing.T) {
	tu := paramTuner(TunerConfig{}, listVariants("orders", 20, 1000), 170004)
	got := tu.parameterizationFindings()
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Category != "query_parameterization" || f.ObjectType != "query_shape" ||
		f.Severity != "warning" {
		t.Fatalf("finding = %s/%s/%s", f.Category, f.ObjectType, f.Severity)
	}
	if !shapeIdent.MatchString(f.ObjectIdentifier) {
		t.Errorf("identifier = %q, want shape:<16 hex>", f.ObjectIdentifier)
	}
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Errorf("parameterization advice carries SQL: %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	want := "SELECT id, total FROM orders WHERE id IN (...)"
	if f.Detail["normalized_query"] != want {
		t.Errorf("normalized_query = %v, want %q", f.Detail["normalized_query"], want)
	}
	if f.Detail["reason"] != "in_list_variants" || f.Detail["variant_count"] != 20 {
		t.Errorf("reason/variants = %v/%v", f.Detail["reason"], f.Detail["variant_count"])
	}
	ids, _ := f.Detail["queryids"].([]int64)
	if len(ids) != 20 || ids[0] != 1001 || ids[19] != 1020 {
		t.Errorf("queryids = %v, want 1001..1020", ids)
	}
	if f.Detail["calls"] != int64(200) || f.Detail["pg_major"] != 17 {
		t.Errorf("calls/pg_major = %v/%v", f.Detail["calls"], f.Detail["pg_major"])
	}
	if ms, _ := f.Detail["total_exec_ms"].(float64); math.Abs(ms-50) > 1e-9 {
		t.Errorf("total_exec_ms = %v, want 50", f.Detail["total_exec_ms"])
	}
	share, _ := f.Detail["plan_time_share"].(float64)
	if math.Abs(share-20.0/70.0) > 1e-9 {
		t.Errorf("plan_time_share = %v, want %v", share, 20.0/70.0)
	}
	if _, ok := f.Detail["hint_directive"]; ok {
		t.Error("parameterization finding carries a hint_directive")
	}
	if !strings.Contains(f.Recommendation, "= ANY($1)") ||
		!strings.Contains(f.Recommendation, "prepared statements") {
		t.Errorf("recommendation = %q", f.Recommendation)
	}
}

// Boundary: 19 variants are below the default threshold.
func TestParameterization_NineteenVariantsEmitNone(t *testing.T) {
	tu := paramTuner(TunerConfig{}, listVariants("orders", 19, 1000), 170004)
	if got := tu.parameterizationFindings(); len(got) != 0 {
		t.Fatalf("findings = %d, want 0: %+v", len(got), got)
	}
}

// Variants of two shapes emit two findings with distinct identities, and
// = ANY (ARRAY[...]) lists collapse like IN lists.
func TestParameterization_TwoShapesEmitTwo(t *testing.T) {
	qs := listVariants("orders", 20, 1000)
	for i := 1; i <= 20; i++ {
		params := make([]string, i)
		for p := range params {
			params[p] = fmt.Sprintf("$%d", p+1)
		}
		qs = append(qs, collector.QueryStats{QueryID: int64(5000 + i), Calls: 1,
			Query: "SELECT * FROM items WHERE sku = ANY (ARRAY[" +
				strings.Join(params, ",") + "])"})
	}
	got := paramTuner(TunerConfig{}, qs, 140010).parameterizationFindings()
	if len(got) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(got), got)
	}
	if got[0].ObjectIdentifier == got[1].ObjectIdentifier {
		t.Fatalf("two shapes share identifier %q", got[0].ObjectIdentifier)
	}
	shapes := map[any]bool{}
	for _, f := range got {
		shapes[f.Detail["normalized_query"]] = true
	}
	if !shapes["SELECT * FROM items WHERE sku = ANY (...)"] {
		t.Errorf("ANY(ARRAY) shape missing: %v", shapes)
	}
}

// The identifier is stable across cycles for the same shape.
func TestParameterization_IdentifierStable(t *testing.T) {
	a := paramTuner(TunerConfig{}, listVariants("orders", 20, 1000), 170004).
		parameterizationFindings()
	b := paramTuner(TunerConfig{}, listVariants("orders", 25, 9000), 170004).
		parameterizationFindings()
	if len(a) != 1 || len(b) != 1 || a[0].ObjectIdentifier != b[0].ObjectIdentifier {
		t.Fatalf("identifiers differ across cycles: %v / %v", a, b)
	}
}

// Boundary: queryids are capped at 20 while variant_count stays exact.
func TestParameterization_QueryIDsCapped(t *testing.T) {
	got := paramTuner(TunerConfig{}, listVariants("orders", 30, 1000), 170004).
		parameterizationFindings()
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	ids, _ := got[0].Detail["queryids"].([]int64)
	if got[0].Detail["variant_count"] != 30 || len(ids) != 20 {
		t.Fatalf("variant_count=%v queryids=%d, want 30 and 20",
			got[0].Detail["variant_count"], len(ids))
	}
}

// The same queryid listed twice (two snapshot rows) is one variant.
func TestParameterization_DuplicateQueryIDCountsOnce(t *testing.T) {
	qs := listVariants("orders", 19, 1000)
	qs = append(qs, qs[0])
	if got := paramTuner(TunerConfig{}, qs, 170004).parameterizationFindings(); len(got) != 0 {
		t.Fatalf("a repeated queryid made a 20th variant: %+v", got)
	}
}

// State: on PG18 pg_stat_statements squashes lists, so the trigger is off.
func TestParameterization_PG18EmitsNone(t *testing.T) {
	tu := paramTuner(TunerConfig{}, listVariants("orders", 40, 1000), 180001)
	if got := tu.parameterizationFindings(); len(got) != 0 {
		t.Fatalf("PG18 findings = %d, want 0: %+v", len(got), got)
	}
}

// State transition: the same tuner, re-pointed at a PG18 server, stops.
func TestParameterization_VersionChangeStopsTrigger(t *testing.T) {
	src := fakeQuerySource{snap: &collector.Snapshot{Queries: listVariants("o", 20, 1)}}
	tu := New(nil, TunerConfig{}, nil, noopLogFn)
	tu.WithQuerySource(src, 170000)
	if got := tu.parameterizationFindings(); len(got) != 1 {
		t.Fatalf("PG17 findings = %d, want 1", len(got))
	}
	tu.WithQuerySource(src, 180000)
	if got := tu.parameterizationFindings(); len(got) != 0 {
		t.Fatalf("PG18 findings = %d, want 0", len(got))
	}
}

// Invalid input: subqueries and literal (unnormalized) lists are not lists.
func TestParameterization_SubqueryAndLiteralListsIgnored(t *testing.T) {
	var qs []collector.QueryStats
	for i := 1; i <= 25; i++ {
		lits := make([]string, i)
		for p := range lits {
			lits[p] = fmt.Sprint(p + 1)
		}
		qs = append(qs,
			collector.QueryStats{QueryID: int64(i), Calls: 1,
				Query: "SELECT * FROM t WHERE id IN (" + strings.Join(lits, ", ") + ")"},
			collector.QueryStats{QueryID: int64(100 + i), Calls: 1,
				Query: fmt.Sprintf("SELECT * FROM t WHERE id IN (SELECT id FROM u "+
					"WHERE n = $1) /* v%d */", i)})
	}
	if got := paramTuner(TunerConfig{}, qs, 160000).parameterizationFindings(); len(got) != 0 {
		t.Fatalf("findings = %d, want 0: %+v", len(got), got)
	}
}

// pg_sage's own statements are never workload advice.
func TestParameterization_OwnStatementsIgnored(t *testing.T) {
	qs := listVariants("sage.findings", 25, 1)
	for i := range qs {
		qs[i].Query = "/* pg_sage */ " + qs[i].Query
	}
	if got := paramTuner(TunerConfig{}, qs, 170000).parameterizationFindings(); len(got) != 0 {
		t.Fatalf("own statements produced %d findings", len(got))
	}
}

func TestCollapseLists(t *testing.T) {
	cases := []struct {
		in, want string
		list     bool
	}{
		{"SELECT 1 FROM t WHERE a IN ($1, $2)", "SELECT 1 FROM t WHERE a IN (...)", true},
		{"select 1 from t where a in ($1)", "select 1 from t where a in (...)", true},
		{"SELECT 1 FROM t WHERE a NOT IN ($1,$2,$3)", "SELECT 1 FROM t WHERE a NOT IN (...)",
			true},
		{"SELECT 1 FROM t WHERE a IN ($1::int, $2::int)", "SELECT 1 FROM t WHERE a IN (...)",
			true},
		{"SELECT 1 FROM t WHERE a = ANY (ARRAY[$1, $2])", "SELECT 1 FROM t WHERE a = ANY (...)",
			true},
		{"SELECT 1 FROM t WHERE a = any(array[$1])", "SELECT 1 FROM t WHERE a = any (...)", true},
		{"SELECT 1 FROM t WHERE a IN (SELECT b FROM u)",
			"SELECT 1 FROM t WHERE a IN (SELECT b FROM u)", false},
		{"SELECT 1 FROM t WHERE a IN (1, 2)", "SELECT 1 FROM t WHERE a IN (1, 2)", false},
		{"SELECT 1 FROM t WHERE a = $1", "SELECT 1 FROM t WHERE a = $1", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, list := collapseLists(c.in)
		if got != c.want || list != c.list {
			t.Errorf("collapseLists(%q) = %q,%v; want %q,%v", c.in, got, list, c.want, c.list)
		}
	}
}

// Nil/empty: no source, no snapshot or no queries mean no finding.
func TestParameterization_NilAndEmptySnapshot(t *testing.T) {
	bare := New(nil, TunerConfig{}, nil, noopLogFn)
	if got := bare.parameterizationFindings(); len(got) != 0 {
		t.Fatalf("no source: %d findings", len(got))
	}
	nilSnap := New(nil, TunerConfig{}, nil, noopLogFn)
	nilSnap.WithQuerySource(fakeQuerySource{}, 170000)
	if got := nilSnap.parameterizationFindings(); len(got) != 0 {
		t.Fatalf("nil snapshot: %d findings", len(got))
	}
	if got := paramTuner(TunerConfig{}, nil, 170000).parameterizationFindings(); len(got) != 0 {
		t.Fatalf("empty snapshot: %d findings", len(got))
	}
}

// Boundary: param_shape_min_variants 0 keeps 20; an explicit 2 fires at 2.
func TestParameterization_MinVariantsConfig(t *testing.T) {
	if got := paramTuner(TunerConfig{ParamShapeMinVariants: 0},
		listVariants("o", 19, 1), 170000).parameterizationFindings(); len(got) != 0 {
		t.Fatalf("unset threshold fired at 19 variants")
	}
	cfg := TunerConfig{ParamShapeMinVariants: 2}
	if got := paramTuner(cfg, listVariants("o", 1, 1), 170000).
		parameterizationFindings(); len(got) != 0 {
		t.Fatalf("threshold 2 fired at 1 variant")
	}
	if got := paramTuner(cfg, listVariants("o", 2, 1), 170000).
		parameterizationFindings(); len(got) != 1 {
		t.Fatalf("threshold 2 did not fire at 2 variants: %d", len(got))
	}
}

func highPlanTimeCandidate() candidate {
	return candidate{QueryID: 4242, Query: "SELECT * FROM accounts WHERE id = $1",
		Calls: 200, MeanExecTime: 5, MeanPlanTime: 45}
}

var planTimeCfg = TunerConfig{MinQueryCalls: 100, PlanTimeRatio: 3}

// Happy path: a high plan-time candidate yields query_parameterization with
// reason plan_time_share and no hint (no plan_cache_mode hint is installed).
func TestHighPlanTime_YieldsParameterizationNotHint(t *testing.T) {
	tu := New(nil, planTimeCfg, &HintPlanAvailability{ApplicationEffect: true}, noopLogFn)
	got := tu.processCandidate(context.Background(), highPlanTimeCandidate())
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Category != "query_parameterization" || f.ObjectType != "query_shape" {
		t.Fatalf("finding = %s/%s, want query_parameterization/query_shape",
			f.Category, f.ObjectType)
	}
	if f.Detail["reason"] != "plan_time_share" {
		t.Errorf("reason = %v, want plan_time_share", f.Detail["reason"])
	}
	if _, ok := f.Detail["hint_directive"]; ok || f.RecommendedSQL != "" {
		t.Errorf("plan-time finding carries a hint: %v / %q",
			f.Detail["hint_directive"], f.RecommendedSQL)
	}
	if share, _ := f.Detail["plan_time_share"].(float64); math.Abs(share-0.9) > 1e-9 {
		t.Errorf("plan_time_share = %v, want 0.9", f.Detail["plan_time_share"])
	}
	ids, _ := f.Detail["queryids"].([]int64)
	if len(ids) != 1 || ids[0] != 4242 || f.Detail["variant_count"] != 1 {
		t.Errorf("queryids = %v variant_count = %v", ids, f.Detail["variant_count"])
	}
	if f.Detail["calls"] != int64(200) || f.Detail["total_exec_ms"] != float64(1000) {
		t.Errorf("calls/total_exec_ms = %v/%v", f.Detail["calls"], f.Detail["total_exec_ms"])
	}
	if f.Detail["normalized_query"] != "SELECT * FROM accounts WHERE id = $1" {
		t.Errorf("normalized_query = %v", f.Detail["normalized_query"])
	}
}

// Boundary: below MinQueryCalls there is no plan-time trigger.
func TestHighPlanTime_BelowMinCallsNoFinding(t *testing.T) {
	c := highPlanTimeCandidate()
	c.Calls = 99
	tu := New(nil, planTimeCfg, nil, noopLogFn)
	if got := tu.processCandidate(context.Background(), c); len(got) != 0 {
		t.Fatalf("findings = %d, want 0: %+v", len(got), got)
	}
	c.Calls = 100
	if got := tu.processCandidate(context.Background(), c); len(got) != 1 {
		t.Fatalf("at MinQueryCalls: findings = %d, want 1", len(got))
	}
}

// Nil/zero: mean_plan_time 0 (track_planning off) is no plan-time trigger.
func TestHighPlanTime_ZeroPlanTimeNoTrigger(t *testing.T) {
	c := highPlanTimeCandidate()
	c.MeanPlanTime = 0
	tu := New(nil, planTimeCfg, nil, noopLogFn)
	if got := tu.processCandidate(context.Background(), c); len(got) != 0 {
		t.Fatalf("findings = %d, want 0: %+v", len(got), got)
	}
}

// A plan-time statement whose shape the list trigger already reported this
// cycle is not reported twice under the same identity.
func TestHighPlanTime_ShapeAlreadyReportedNotDuplicated(t *testing.T) {
	qs := listVariants("orders", 20, 1000)
	tu := paramTuner(planTimeCfg, qs, 170000)
	if got := tu.parameterizationFindings(); len(got) != 1 {
		t.Fatalf("list findings = %d, want 1", len(got))
	}
	c := highPlanTimeCandidate()
	c.QueryID, c.Query = qs[4].QueryID, qs[4].Query
	if got := paramFindings(tu.processCandidate(context.Background(), c)); len(got) != 0 {
		t.Fatalf("shape reported twice: %+v", got)
	}
}

// The next cycle starts with an empty set of reported shapes.
func TestParameterization_EmittedResetEachCycle(t *testing.T) {
	tu := paramTuner(planTimeCfg, listVariants("orders", 20, 1000), 170000)
	first := tu.parameterizationFindings()
	second := tu.parameterizationFindings()
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("cycles = %d then %d findings, want 1 and 1", len(first), len(second))
	}
}
