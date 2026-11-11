package tuner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// tuning-05: the pure parts of the hinted EXPLAIN gate.

func sidesOf(plainCost, hintedCost float64, same bool) (planSummary, planSummary) {
	h := "v2u:hinted"
	if same {
		h = "v2u:plain"
	}
	return planSummary{hash: "v2u:plain", cost: plainCost}, planSummary{hash: h, cost: hintedCost}
}

// Boundary: tolerance 10 admits +9.9% and refuses +10.1%; equal cost with a
// changed plan is admitted; a tenfold rise is refused.
func TestJudgeHint_CostToleranceBoundary(t *testing.T) {
	cases := []struct {
		name   string
		hinted float64
		want   HintVerdict
	}{
		{"equal cost, changed plan", 100, HintChanged},
		{"+9.9%", 109.9, HintChanged},
		{"exactly +10%", 110, HintChanged},
		{"+10.1%", 110.1, HintCostRegression},
		{"tenfold", 1000, HintCostRegression},
		{"cheaper", 50, HintChanged},
	}
	for _, c := range cases {
		p, h := sidesOf(100, c.hinted, false)
		got, basis, reason := judgeHint(p, h, "HashJoin(c o)", 10, false)
		if got != c.want || basis != "plan" || reason == "" {
			t.Errorf("%s: verdict %q basis %q reason %q, want %q/plan", c.name, got, basis,
				reason, c.want)
		}
	}
}

// Boundary: tolerance 0 refuses any rise, 100 admits a doubling only.
func TestJudgeHint_ToleranceZeroAndHundred(t *testing.T) {
	p, h := sidesOf(100, 100.01, false)
	if got, _, _ := judgeHint(p, h, "SeqScan(o)", 0, false); got != HintCostRegression {
		t.Fatalf("tolerance 0, +0.01%%: %q, want cost_regression", got)
	}
	p, h = sidesOf(100, 200, false)
	if got, _, _ := judgeHint(p, h, "SeqScan(o)", 100, false); got != HintChanged {
		t.Fatalf("tolerance 100, +100%%: %q, want changed", got)
	}
	p, h = sidesOf(100, 200.5, false)
	if got, _, _ := judgeHint(p, h, "SeqScan(o)", 100, false); got != HintCostRegression {
		t.Fatalf("tolerance 100, +100.5%%: %q, want cost_regression", got)
	}
}

// Invalid input: an unknown index plans the same, so the hint has no effect.
func TestJudgeHint_SamePlanIsNoEffect(t *testing.T) {
	p, h := sidesOf(100, 100, true)
	got, _, reason := judgeHint(p, h, "IndexScan(o idx_missing)", 10, true)
	if got != HintNoEffect || !strings.Contains(reason, "plain plan") {
		t.Fatalf("verdict %q reason %q, want no_effect", got, reason)
	}
	if !got.Refused() {
		t.Fatal("no_effect must refuse")
	}
}

// Invalid input (PG14, pg_hint_plan 1.4): an unknown index forces a
// disabled path; that hint has no effect, not a cost regression.
func TestJudgeHint_DisabledPathIsNoEffect(t *testing.T) {
	plain := planSummary{hash: "v2u:a", cost: 268}
	hinted := planSummary{hash: "v2u:b", cost: 1.0e10 + 2049, disabled: true}
	if got, _, reason := judgeHint(plain, hinted, "IndexScan(o idx_missing)", 10,
		false); got != HintNoEffect || !strings.Contains(reason, "disabled") {
		t.Fatalf("disabled hinted path: %q (%s), want no_effect", got, reason)
	}
	plain.disabled, plain.cost = true, 1.0e10+2000
	if got, _, _ := judgeHint(plain, hinted, "SeqScan(o)", 10, false); got !=
		HintChanged {
		t.Fatalf("both sides disabled is judged by cost: %q, want changed", got)
	}
	for plan, want := range map[string]bool{
		`[{"Plan":{"Node Type":"Seq Scan","Total Cost":10000000123.5}}]`:      true,
		`[{"Plan":{"Node Type":"Seq Scan","Total Cost":12,"Disabled":true}}]`: true,
		`[{"Plan":{"Node Type":"Hash","Total Cost":12,"Plans":[{"Node Type":"Seq` +
			` Scan","Disabled":true}]}}]`: true,
		`[{"Plan":{"Node Type":"Seq Scan","Total Cost":12}}]`: false,
	} {
		s, err := summarizePlan([]byte(plan))
		if err != nil || s.disabled != want {
			t.Errorf("summarizePlan(%s).disabled = %t (%v), want %t", plan, s.disabled, err,
				want)
		}
	}
}

// Set(work_mem): an unchanged shape is admitted only on a spilling ANALYZE
// plan (basis spill), otherwise advisory; a Set-only hint is never a cost
// regression; a work_mem hint joined with a scan hint is judged as a scan.
func TestJudgeHint_WorkMem(t *testing.T) {
	p, h := sidesOf(100, 100, true)
	if got, basis, _ := judgeHint(p, h, `Set(work_mem "256MB")`, 10, true); got != HintChanged ||
		basis != "spill" {
		t.Fatalf("spill evidence: %q/%q, want changed/spill", got, basis)
	}
	got, _, reason := judgeHint(p, h, `Set(work_mem "256MB")`, 10, false)
	if got != HintUnverifiable || !strings.Contains(reason, "spill") || got.Refused() {
		t.Fatalf("no spill evidence: %q (%s), want unverifiable, not refused", got, reason)
	}
	p, h = sidesOf(100, 500, false)
	if got, _, _ := judgeHint(p, h, `Set(work_mem "256MB")`, 10, false); got != HintChanged {
		t.Fatalf("Set-only plan change with cost rise: %q, want changed", got)
	}
	if got, _, _ := judgeHint(p, h, `Set(work_mem "256MB") SeqScan(o)`, 10, false); got !=
		HintCostRegression {
		t.Fatalf("work_mem + scan hint, 5x cost: %q, want cost_regression", got)
	}
	p, h = sidesOf(100, 100, true)
	if got, _, _ := judgeHint(p, h, `Set(work_mem "256MB") SeqScan(o)`, 10, true); got !=
		HintNoEffect {
		t.Fatalf("work_mem + scan hint, same plan: %q, want no_effect", got)
	}
}

func TestHintVerdictRefused(t *testing.T) {
	for v, refused := range map[HintVerdict]bool{
		HintChanged: false, HintUnverifiable: false, HintNoEffect: true,
		HintCostRegression: true, HintExplainTimeout: true, HintExplainFailed: true,
		HintInvalidStatement: true, HintVerdict(""): true,
	} {
		if v.Refused() != refused {
			t.Errorf("%q.Refused() = %t, want %t", v, v.Refused(), refused)
		}
	}
}

// Invalid input: multi-statement text, empty text, non-explainable
// statements and hints that could leave their comment are refused.
func TestExplainableBody(t *testing.T) {
	refusedCases := []struct{ stmt, hint, want string }{
		{"SELECT 1; SELECT 2", "SeqScan(t)", "multi-statement"},
		{"SELECT ';'", "SeqScan(t)", "multi-statement"},
		{"", "SeqScan(t)", "empty"},
		{"  ;  ", "SeqScan(t)", "empty"},
		{"VACUUM t", "SeqScan(t)", "not a SELECT"},
		{"EXPLAIN ANALYZE SELECT 1", "SeqScan(t)", "not a SELECT"},
		{"/* x */ ANALYZE t", "SeqScan(t)", "not a SELECT"},
		{"SELECT 1", "HashJoin(a */ SELECT 1 /* b)", "comment body"},
		{"SELECT 1", "DROP TABLE t", "comment body"},
		{"SELECT 1", "", "comment body"},
	}
	for _, c := range refusedCases {
		if _, why := explainableBody(c.stmt, c.hint); !strings.Contains(why, c.want) {
			t.Errorf("explainableBody(%q, %q) why = %q, want %q", c.stmt, c.hint, why, c.want)
		}
	}
	body, why := explainableBody("  WITH x AS (SELECT 1) SELECT * FROM x;  ", "SeqScan(x)")
	if why != "" || body != "WITH x AS (SELECT 1) SELECT * FROM x" {
		t.Fatalf("valid WITH: body %q why %q", body, why)
	}
	if _, why := explainableBody("(SELECT 1)", "SeqScan(x)"); why != "" {
		t.Fatalf("parenthesized SELECT refused: %s", why)
	}
}

// Invalid input refuses before any database work: a nil database is never
// touched (it would fail with explain_failed instead).
func TestCompareHint_RefusesBeforeExplain(t *testing.T) {
	cmp, err := CompareHint(context.Background(), nil, "SELECT 1; DROP TABLE t",
		"SeqScan(t)", 17, CompareOptions{})
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintInvalidStatement {
		t.Fatalf("multi-statement: %q, %v, want invalid_statement", cmp.Verdict, err)
	}
	cmp, err = CompareHint(context.Background(), nil, "SELECT * FROM t WHERE a = $1",
		"SeqScan(t)", 14, CompareOptions{TolerancePct: 10})
	if err != nil || cmp.Verdict != HintUnverifiable || cmp.PGMajor != 14 ||
		!strings.Contains(cmp.Reason, "PostgreSQL 16") {
		t.Fatalf("parameterized on PG14: %+v, %v, want unverifiable", cmp, err)
	}
	cmp, err = CompareHint(context.Background(), nil, "/*+ SeqScan(t) */ SELECT * FROM t",
		"IndexScan(t)", 17, CompareOptions{})
	if err != nil || cmp.Verdict != HintUnverifiable {
		t.Fatalf("own hint comment: %+v, %v, want unverifiable", cmp, err)
	}
	// A literal $1 is not a bind parameter: it reaches the database (nil
	// here), so the refusal is explain_failed, not unverifiable.
	cmp, err = CompareHint(context.Background(), nil, "SELECT '$1'", "SeqScan(t)", 14,
		CompareOptions{})
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintExplainFailed {
		t.Fatalf("literal $1 on PG14 with no database: %q, %v, want explain_failed",
			cmp.Verdict, err)
	}
}

func TestHasBindParam(t *testing.T) {
	for q, want := range map[string]bool{
		"SELECT * FROM t WHERE a = $1": true, "SELECT '$1'": false,
		"SELECT 1 /* $2 */": false, "SELECT 1": false, `SELECT "$3"`: true,
	} {
		if got := hasBindParam(q); got != want {
			t.Errorf("hasBindParam(%q) = %t, want %t", q, got, want)
		}
	}
}

func TestPlanSpills(t *testing.T) {
	cases := map[string]bool{
		`[{"Plan":{"Node Type":"Sort","Sort Space Type":"Disk"}}]`:   true,
		`[{"Plan":{"Node Type":"Sort","Sort Space Type":"Memory"}}]`: false,
		`[{"Plan":{"Node Type":"Hash Join","Plans":[{"Node Type":"Hash",` +
			`"Hash Batches":4}]}}]`: true,
		`[{"Plan":{"Node Type":"Hash","Hash Batches":1}}]`: false,
		`not json`: false,
		``:         false,
	}
	for plan, want := range cases {
		if got := planSpills([]byte(plan)); got != want {
			t.Errorf("planSpills(%s) = %t, want %t", plan, got, want)
		}
	}
}

func TestSummarizePlan(t *testing.T) {
	s, err := summarizePlan([]byte(`[{"Plan":{"Node Type":"Seq Scan",` +
		`"Relation Name":"t","Total Cost":12.5}}]`))
	if err != nil || s.cost != 12.5 || !strings.HasPrefix(s.hash, "v2u:") {
		t.Fatalf("summary %+v, %v", s, err)
	}
	for _, bad := range []string{``, `[]`, `{"x":1}`, `[{"Plan":{}}]`} {
		if _, err := summarizePlan([]byte(bad)); err == nil {
			t.Errorf("summarizePlan(%q) accepted", bad)
		}
	}
}

// Evidence carries the verdict, costs, hashes, delta and both plans; a plan
// over 64 KB is left out and named.
func TestHintComparisonEvidence(t *testing.T) {
	cmp := HintComparison{Verdict: HintChanged, Basis: "plan", Reason: "r", PGMajor: 17,
		Generic: true, TolerancePct: 10, PlainCost: 200, HintedCost: 210,
		PlainHash: "v2u:a", HintedHash: "v2u:b", PlainPlan: []byte(`[{"Plan":{}}]`),
		HintedPlan: []byte(`[` + strings.Repeat(" ", maxEvidencePlanBytes) + `]`)}
	ev := cmp.Evidence()
	if ev["verdict"] != "changed" || ev["basis"] != "plan" || ev["pg_major"] != 17 ||
		ev["generic_plan"] != true || ev["plain_cost"] != 200.0 || ev["hinted_cost"] != 210.0 ||
		ev["cost_delta_pct"] != 5.0 || ev["plain_plan_hash"] != "v2u:a" ||
		ev["hinted_plan_hash"] != "v2u:b" || ev["tolerance_pct"] != 10.0 {
		t.Fatalf("evidence %v", ev)
	}
	if raw, ok := ev["plain_plan"].(json.RawMessage); !ok || string(raw) != `[{"Plan":{}}]` {
		t.Fatalf("plain plan %v", ev["plain_plan"])
	}
	if _, ok := ev["hinted_plan"]; ok || ev["hinted_plan_omitted"] == nil {
		t.Fatalf("an over-64KB plan must be left out and named: %v", ev["hinted_plan_omitted"])
	}
	if _, err := json.Marshal(ev); err != nil {
		t.Fatalf("evidence does not encode: %v", err)
	}
	// Zero value: verdict only, no costs.
	ev = HintComparison{Verdict: HintUnverifiable, Reason: "x"}.Evidence()
	if ev["verdict"] != "unverifiable" || ev["plain_cost"] != nil || ev["basis"] != nil {
		t.Fatalf("unverifiable evidence %v", ev)
	}
}

// withHintEvidence: a refusal drops the finding, unverifiable drops the SQL.
func TestWithHintEvidence(t *testing.T) {
	f := analyzer.Finding{RecommendedSQL: "INSERT", RollbackSQL: "DELETE"}
	got, err := withHintEvidence(f, HintComparison{Verdict: HintUnverifiable, Reason: "r"}, nil)
	if err != nil || got.RecommendedSQL != "" || got.RollbackSQL != "" ||
		got.Detail["hint_evidence"].(map[string]any)["verdict"] != "unverifiable" {
		t.Fatalf("unverifiable: %+v, %v", got, err)
	}
	got, err = withHintEvidence(f, HintComparison{Verdict: HintChanged}, nil)
	if err != nil || got.RecommendedSQL != "INSERT" || got.RollbackSQL != "DELETE" {
		t.Fatalf("changed: %+v, %v", got, err)
	}
	refusal := errors.New("refused")
	if _, err := withHintEvidence(f, HintComparison{Verdict: HintNoEffect}, refusal); err !=
		refusal {
		t.Fatalf("refusal error not propagated: %v", err)
	}
}

func readyHintTuner(avail *HintPlanAvailability) *Tuner {
	tu := New(nil, TunerConfig{HintCostTolerancePct: 10}, avail, noopLogFn)
	tu.WithHintComparison(170000, catalogread.Default())
	return tu
}

// Nil/empty: without a connection, or with pg_hint_plan neither preloaded nor
// loadable, the gate is unverifiable and never queries.
func TestCompareHint_TunerWithoutModuleOrPool(t *testing.T) {
	tu := readyHintTuner(&HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true, SharedPreload: true})
	cmp, err := tu.compareHint(context.Background(), 1, "SELECT 1", "SeqScan(t)", nil)
	if err != nil || cmp.Verdict != HintUnverifiable || !strings.Contains(cmp.Reason,
		"no connection") || cmp.PGMajor != 17 || cmp.TolerancePct != 10 {
		t.Fatalf("no pool: %+v, %v", cmp, err)
	}
	// A pool that cannot connect: a query would fail, so unverifiable here
	// proves none ran.
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	defer pool.Close()
	for _, avail := range []*HintPlanAvailability{nil, {}, {ApplicationEffect: false}} {
		tu := readyHintTuner(avail)
		tu.pool = pool
		cmp, err := tu.compareHint(context.Background(), 1, "SELECT 1", "SeqScan(t)", nil)
		if err != nil || cmp.Verdict != HintUnverifiable ||
			!strings.Contains(cmp.Reason, "neither preloaded nor loadable") {
			t.Errorf("facts %+v: %+v, %v, want unverifiable without a query", avail, cmp, err)
		}
	}
	if got := New(nil, TunerConfig{}, nil, noopLogFn); got.HintComparisonEnabled() {
		t.Fatal("the gate must be off until WithHintComparison")
	}
	var nilTuner *Tuner
	if nilTuner.HintComparisonEnabled() {
		t.Fatal("nil tuner reports the gate on")
	}
}

// State: the gate off leaves a finding unchanged; HintedExplain refuses.
func TestGateOff(t *testing.T) {
	tu := New(nil, TunerConfig{}, nil, noopLogFn)
	f := analyzer.Finding{RecommendedSQL: "INSERT"}
	got, err := tu.gateFinding(context.Background(), f, 1, "SELECT 1", "SeqScan(t)", nil)
	if err != nil || got.RecommendedSQL != "INSERT" || got.Detail != nil {
		t.Fatalf("gate off changed the finding: %+v, %v", got, err)
	}
	if _, err := tu.HintedExplain(context.Background(), HintProposal{QueryID: 1,
		Hint: "SeqScan(t)"}); !errors.Is(err, ErrHintsUnavailable) {
		t.Fatalf("HintedExplain with the gate off: %v", err)
	}
	if cmp, refused := tu.gateCandidate(context.Background(), candidate{QueryID: 1},
		"SeqScan(t)"); cmp != nil || refused {
		t.Fatalf("gateCandidate with the gate off: %v %t", cmp, refused)
	}
}

// Invalid input: HintedExplain validates the hint like CheckHint.
func TestHintedExplain_InvalidHint(t *testing.T) {
	tu := readyHintTuner(nil)
	if _, err := tu.HintedExplain(context.Background(), HintProposal{QueryID: 1,
		Hint: "DROP TABLE t"}); !errors.Is(err, ErrInvalidHint) {
		t.Fatalf("invalid hint: %v, want ErrInvalidHint", err)
	}
	_, err := tu.HintedExplain(context.Background(), HintProposal{Hint: "SeqScan(t)"})
	if !errors.Is(err, ErrInvalidHint) {
		t.Fatalf("no queryid: %v, want ErrInvalidHint", err)
	}
}

// The gate (no pool: unverifiable) leaves a deterministic hint advisory: no
// SQL, the reason in detail.hint_evidence, and no proposed row (no pool).
func TestBuildGatedFinding_UnverifiableIsAdvisory(t *testing.T) {
	tu := New(nil, TunerConfig{}, &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true, SharedPreload: true}, noopLogFn)
	cmp := unverifiable(HintComparison{PGMajor: 17}, "why")
	f := tu.buildGatedFinding(context.Background(), candidate{QueryID: 5, Query: "SELECT 1"},
		nil, "SeqScan(t)", "t", "r", &cmp)
	ev, _ := f.Detail["hint_evidence"].(map[string]any)
	if f.RecommendedSQL != "" || f.RollbackSQL != "" || ev["verdict"] != "unverifiable" ||
		ev["reason"] != "why" {
		t.Fatalf("unverifiable finding: sql %q detail %v", f.RecommendedSQL, f.Detail)
	}
	changed := HintComparison{Verdict: HintChanged, PlainHash: "a", HintedHash: "b"}
	f = tu.buildGatedFinding(context.Background(), candidate{QueryID: 5, Query: "SELECT 1"},
		nil, "SeqScan(t)", "t", "r", &changed)
	if f.RecommendedSQL == "" || f.Detail["hint_evidence"].(map[string]any)["verdict"] !=
		"changed" {
		t.Fatalf("changed finding: sql %q detail %v", f.RecommendedSQL, f.Detail)
	}
}

// Error propagation: a statement timeout, a lock timeout and the caller's
// deadline are explain_timeout; any other EXPLAIN error is explain_failed.
// Both refuse and keep the comparison's server major.
func TestRefuseExplain_ClassifiesTimeouts(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want HintVerdict
	}{
		{"statement_timeout", &pgconn.PgError{Code: "57014"}, HintExplainTimeout},
		{"lock_timeout", &pgconn.PgError{Code: "55P03"}, HintExplainTimeout},
		{"deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), HintExplainTimeout},
		{"syntax_error", &pgconn.PgError{Code: "42601"}, HintExplainFailed},
		{"conn_closed", errors.New("conn closed"), HintExplainFailed},
	}
	for _, tc := range cases {
		cmp, err := refuseExplain(HintComparison{PGMajor: 17}, "hinted", tc.err)
		if cmp.Verdict != tc.want || !errors.Is(err, ErrHintRefused) || cmp.PGMajor != 17 ||
			!strings.HasPrefix(cmp.Reason, "hinted EXPLAIN: ") {
			t.Errorf("%s: %+v, %v, want %s", tc.name, cmp, err, tc.want)
		}
	}
}
