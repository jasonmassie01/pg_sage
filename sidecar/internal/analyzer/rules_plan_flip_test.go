package analyzer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/planfresh"
)

// guard-02c: a plan regression whose plan hash changed after a routine
// ANALYZE of a relation the new plan reads is a plan flip, fixed by one
// re-ANALYZE (Neon 2025, Clerk 2026). No concurrent access tests in this
// file: the rule's functions are pure; two cycles upserting the same flip
// are covered by TestPlanFlipDB_TwoCyclesOpenOneFinding.

var flipT0 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

func flipAt(d time.Duration) *time.Time {
	t := flipT0.Add(d)
	return &t
}

// flipRegression is the plan_regression finding of a pair whose hashes
// are prev and cur.
func flipRegression(prev, cur string) Finding {
	d := map[string]any{"queryid": int64(42), "query": "SELECT * FROM orders WHERE v = $1",
		"cost_ratio": 8.0, "previous_plan_hash": prev, "current_plan_hash": cur}
	if hashVersion(prev) == hashVersion(cur) {
		d["plan_changed"] = prev != cur
	}
	return Finding{Category: "plan_regression", Severity: "warning",
		ObjectType: "query", ObjectIdentifier: "queryid:42", Detail: d}
}

func flipPair() planPair {
	return planPair{QueryID: 42, QueryText: "SELECT * FROM orders WHERE v = $1",
		PreviousAt: flipT0, CurrentAt: flipT0.Add(2 * time.Hour)}
}

func ordersStats(manual, auto *time.Time) []relStats {
	return []relStats{{Schema: "public", Name: "orders", LastAnalyze: manual,
		LastAutoanalyze: auto}}
}

func TestPlanFlip_CauseIsAnAnalyzeBetweenTheCaptures(t *testing.T) {
	p := flipPair()
	c, ok := flipCause(ordersStats(nil, flipAt(time.Hour)), nil, p.PreviousAt, p.CurrentAt)
	if !ok || c.Relation() != "public.orders" || !c.At.Equal(*flipAt(time.Hour)) ||
		c.By != analyzedByAuto {
		t.Fatalf("cause = %+v, %v; want the autoanalyze of public.orders at +1h", c, ok)
	}
	c, ok = flipCause(ordersStats(flipAt(90*time.Minute), flipAt(time.Hour)), nil,
		p.PreviousAt, p.CurrentAt)
	if !ok || c.By != analyzedByManual || !c.At.Equal(*flipAt(90 * time.Minute)) {
		t.Fatalf("cause = %+v, %v; want the later manual ANALYZE", c, ok)
	}
}

// An analyze at exactly the earlier capture time is not "between": that
// capture may already have been planned with those statistics.
func TestPlanFlip_AnalyzeAtACaptureTimeIsNotBetween(t *testing.T) {
	p := flipPair()
	for name, at := range map[string]*time.Time{
		"at the earlier capture": flipAt(0),
		"at the later capture":   flipAt(2 * time.Hour),
		"before both":            flipAt(-time.Minute),
		"after both":             flipAt(3 * time.Hour),
	} {
		if c, ok := flipCause(ordersStats(nil, at), nil, p.PreviousAt, p.CurrentAt); ok {
			t.Errorf("%s: cause = %+v, want none", name, c)
		}
	}
	if _, ok := flipCause(ordersStats(nil, flipAt(time.Nanosecond)), nil, p.PreviousAt,
		p.CurrentAt); !ok {
		t.Error("an analyze 1ns after the earlier capture is between them")
	}
}

func TestPlanFlip_NoAnalyzeGivesNoCause(t *testing.T) {
	p := flipPair()
	if c, ok := flipCause(nil, nil, p.PreviousAt, p.CurrentAt); ok {
		t.Fatalf("no relations: cause = %+v", c)
	}
	if c, ok := flipCause(ordersStats(nil, nil), nil, p.PreviousAt, p.CurrentAt); ok {
		t.Fatalf("never analyzed: cause = %+v", c)
	}
	if c, ok := flipCause(ordersStats(nil, nil), nil, time.Time{}, time.Time{}); ok {
		t.Fatalf("zero capture times: cause = %+v", c)
	}
}

// pg_sage's own ANALYZE (an executor action) is never the cause: it is
// the fix, or another finding's action.
func TestPlanFlip_PgSageOwnAnalyzeIsNotACause(t *testing.T) {
	p := flipPair()
	own := []sageAnalyze{{Target: "public.orders", At: flipT0.Add(time.Hour + 2*time.Second)}}
	if c, ok := flipCause(ordersStats(flipAt(time.Hour), nil), own, p.PreviousAt,
		p.CurrentAt); ok {
		t.Fatalf("pg_sage's ANALYZE was taken as the cause: %+v", c)
	}
	other := []sageAnalyze{{Target: "public.items", At: flipT0.Add(time.Hour)}}
	c, ok := flipCause(ordersStats(flipAt(time.Hour), nil), other, p.PreviousAt, p.CurrentAt)
	if !ok || c.By != analyzedByManual {
		t.Fatalf("an operator's ANALYZE beside pg_sage's of another table = %+v, %v", c, ok)
	}
	// pg_sage's ANALYZE of orders does not hide an autoanalyze of it.
	c, ok = flipCause(ordersStats(flipAt(time.Hour), flipAt(30*time.Minute)), own,
		p.PreviousAt, p.CurrentAt)
	if !ok || c.By != analyzedByAuto || !c.At.Equal(*flipAt(30 * time.Minute)) {
		t.Fatalf("autoanalyze beside pg_sage's ANALYZE = %+v, %v", c, ok)
	}
}

func TestPlanFlip_LatestAnalyzedRelationIsTheCause(t *testing.T) {
	p := flipPair()
	rels := []relStats{
		{Schema: "public", Name: "orders", LastAutoanalyze: flipAt(20 * time.Minute)},
		{Schema: "sales", Name: "items", LastAutoanalyze: flipAt(80 * time.Minute)},
		{Schema: "public", Name: "users", LastAutoanalyze: flipAt(-time.Hour)},
	}
	c, ok := flipCause(rels, nil, p.PreviousAt, p.CurrentAt)
	if !ok || c.Relation() != "sales.items" {
		t.Fatalf("cause = %+v, %v; want sales.items", c, ok)
	}
}

// Fingerprints of different versions (resolved v2, unresolved v2u) are
// never compared, so no flip is claimed from them.
func TestPlanFlip_DifferentHashVersionsAreNotCompared(t *testing.T) {
	if _, _, ok := flipHashes(flipRegression("v2:aaaa", "v2u:bbbb")); ok {
		t.Fatal("v2 and v2u hashes were compared")
	}
	if _, _, ok := flipHashes(flipRegression("v2:aaaa", "v2:aaaa")); ok {
		t.Fatal("an unchanged hash is a flip")
	}
	prev, cur, ok := flipHashes(flipRegression("v2:aaaa", "v2:bbbb"))
	if !ok || prev != "v2:aaaa" || cur != "v2:bbbb" {
		t.Fatalf("hashes = %q, %q, %v", prev, cur, ok)
	}
	if _, _, ok := flipHashes(Finding{Detail: map[string]any{}}); ok {
		t.Fatal("a finding without hashes is a flip")
	}
	if _, _, ok := flipHashes(Finding{}); ok {
		t.Fatal("a nil detail is a flip")
	}
}

func flipFinding(t *testing.T, sage []sageAnalyze, now time.Time) Finding {
	t.Helper()
	before, after := 12.0, 4800.0
	c := flipCauseT{Schema: "public", Name: "orders", At: flipT0.Add(time.Hour),
		By: analyzedByAuto}
	return buildPlanFlipFinding(flipRegression("v2:aaaa", "v2:bbbb"), flipPair(), c,
		blocksPerCall{Before: &before, After: &after}, sage, now)
}

func TestPlanFlip_FindingCarriesBothHashesAndTheRelation(t *testing.T) {
	f := flipFinding(t, nil, flipT0.Add(3*time.Hour))
	d := f.Detail
	if f.Category != categoryPlanFlip || f.ObjectIdentifier != "queryid:42" ||
		f.Severity != "warning" || f.ObjectType != "query" {
		t.Fatalf("finding = %+v", f)
	}
	if d["previous_plan_hash"] != "v2:aaaa" || d["current_plan_hash"] != "v2:bbbb" ||
		d["relation"] != "public.orders" || d["analyzed_by"] != analyzedByAuto ||
		d["queryid"] != int64(42) {
		t.Fatalf("detail = %v", d)
	}
	if d["analyzed_at"] != flipT0.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("analyzed_at = %v", d["analyzed_at"])
	}
	if b, ok := d["blocks_per_call_before"].(float64); !ok || b != 12 {
		t.Fatalf("blocks_per_call_before = %v", d["blocks_per_call_before"])
	}
	if b, ok := d["blocks_per_call_after"].(float64); !ok || b != 4800 {
		t.Fatalf("blocks_per_call_after = %v", d["blocks_per_call_after"])
	}
	if f.RecommendedSQL != `ANALYZE "public"."orders";` || f.ActionRisk != "safe" {
		t.Fatalf("sql = %q risk = %q", f.RecommendedSQL, f.ActionRisk)
	}
	if !strings.Contains(f.Recommendation, "public.orders") ||
		!strings.Contains(f.Title, "42") {
		t.Fatalf("title %q recommendation %q", f.Title, f.Recommendation)
	}
}

func TestPlanFlip_QuotesTheRelationName(t *testing.T) {
	c := flipCauseT{Schema: "Sales", Name: `odd"name`, At: flipT0.Add(time.Hour)}
	f := buildPlanFlipFinding(flipRegression("v2:a", "v2:b"), flipPair(), c,
		blocksPerCall{}, nil, flipT0.Add(3*time.Hour))
	if f.RecommendedSQL != `ANALYZE "Sales"."odd""name";` {
		t.Fatalf("sql = %q", f.RecommendedSQL)
	}
	if _, ok := f.Detail["blocks_per_call_before"]; ok {
		t.Fatal("an unknown blocks per call was recorded")
	}
}

// Never a second automatic re-ANALYZE within 24 h of pg_sage's first.
func TestPlanFlip_NoSecondReanalyzeWithin24h(t *testing.T) {
	now := flipT0.Add(30 * time.Hour)
	cases := []struct {
		name    string
		ago     time.Duration
		target  string
		propose bool
	}{
		{"23h ago", 23 * time.Hour, "public.orders", false},
		{"just under 24h", 24*time.Hour - time.Second, "orders", false},
		{"exactly 24h", 24 * time.Hour, "public.orders", true},
		{"25h ago", 25 * time.Hour, "public.orders", true},
		{"another table 1h ago", time.Hour, "public.items", true},
	}
	for _, tc := range cases {
		f := flipFinding(t, []sageAnalyze{{Target: tc.target, At: now.Add(-tc.ago)}}, now)
		if got := f.RecommendedSQL != ""; got != tc.propose {
			t.Errorf("%s: proposed = %v (sql %q), want %v", tc.name, got, f.RecommendedSQL,
				tc.propose)
		}
		if _, held := f.Detail["reanalyze_held_until"]; held == tc.propose {
			t.Errorf("%s: reanalyze_held_until present = %v", tc.name, held)
		}
	}
}

func TestPlanFlip_AnalyzeTarget(t *testing.T) {
	for sql, want := range map[string]string{
		"ANALYZE public.orders;":                    "public.orders",
		`analyze verbose "Public"."Orders" (a, b)`:  "public.orders",
		"ANALYZE (VERBOSE, SKIP_LOCKED) orders":     "orders",
		"  ANALYZE   sales.items  ":                 "sales.items",
		`ANALYZE "odd""name"`:                       `odd"name`,
		"VACUUM ANALYZE public.orders":              "",
		"ANALYZE":                                   "",
		"":                                          "",
		"ANALYZE public.a, public.b":                "",
		"SELECT 1; ANALYZE public.orders":           "",
		"ANALYZE (VERBOSE public.orders":            "",
		"ANALYZE public.orders; DROP TABLE x":       "",
		"ANALYZE \"unterminated":                    "",
		"ANALYZE verbose":                           "",
		"ANALYZE public.orders(a)":                  "public.orders",
		"ANALYZE /* c */ public.orders":             "",
		"ANALYZE public . orders":                   "",
		"ANALYZE public.orders.extra.too.many.dots": "",
	} {
		if got := analyzeTarget(sql); got != want {
			t.Errorf("analyzeTarget(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestPlanFlip_SageAnalyzeNames(t *testing.T) {
	s := sageAnalyze{Target: "orders"}
	if !s.names("public", "orders") || !s.names("PUBLIC", "Orders") {
		t.Fatal("an unqualified target names every same-named table (fail closed)")
	}
	q := sageAnalyze{Target: "public.orders"}
	if !q.names("public", "orders") || q.names("sales", "orders") || q.names("", "orders") {
		t.Fatal("a qualified target names only its schema's table")
	}
	if (sageAnalyze{}).names("public", "orders") {
		t.Fatal("an empty target names a table")
	}
}

// fakeFlipDB is a planFlipDB whose reads are scripted.
type fakeFlipDB struct {
	rels     []relStats
	relErr   error
	sage     []sageAnalyze
	sageErr  error
	now      time.Time
	blocks   blocksPerCall
	open     []openFlip
	openErr  error
	latest   map[int64]*latestCapture
	recorded map[int64]map[string]any
}

func (f *fakeFlipDB) relations(context.Context, planfresh.Names) ([]relStats, error) {
	return f.rels, f.relErr
}

func (f *fakeFlipDB) sageAnalyzes(context.Context) ([]sageAnalyze, time.Time, error) {
	return f.sage, f.now, f.sageErr
}

func (f *fakeFlipDB) blocksPerCall(context.Context, int64, time.Time) (blocksPerCall, error) {
	return f.blocks, nil
}

func (f *fakeFlipDB) openFlips(context.Context) ([]openFlip, error) {
	return f.open, f.openErr
}

func (f *fakeFlipDB) latestCapture(_ context.Context, q int64) (*latestCapture, error) {
	return f.latest[q], nil
}

func (f *fakeFlipDB) recordVerdict(_ context.Context, id int64, d map[string]any) error {
	if f.recorded == nil {
		f.recorded = map[int64]map[string]any{}
	}
	f.recorded[id] = d
	return nil
}

// flipPairs is one regressed pair (Index Scan -> Seq Scan, 4x the cost)
// captured around an autoanalyze of orders.
func flipPairs(t *testing.T) []planPair {
	t.Helper()
	p := flipPair()
	p.PreviousPlan, p.CurrentPlan = []byte(planJSON(t, "Index Scan")),
		[]byte(planJSON(t, "Seq Scan"))
	p.PreviousCost, p.CurrentCost = 100, 400
	return []planPair{p}
}

func flipAnalyzer(db planFlipDB) *Analyzer {
	a := &Analyzer{logFn: noopLog, flipDB: db}
	a.eval = newCycleEval()
	return a
}

// A failed relation lookup leaves the rule unevaluated for that query:
// its open finding is kept, not resolved, and nothing is emitted for it.
func TestPlanFlip_FailedRelationLookupKeepsTheQueryUnevaluated(t *testing.T) {
	db := &fakeFlipDB{relErr: errors.New("permission denied for pg_stat_user_tables"),
		now: flipT0.Add(3 * time.Hour)}
	a := flipAnalyzer(db)
	got := a.checkPlanFlips(context.Background(), flipPairs(t), true)
	if len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
	if !a.eval.kept(categoryPlanFlip)["queryid:42"] {
		t.Fatal("the query's open flip finding was not kept")
	}
	if a.eval.hasFailed(categoryPlanFlip) || !a.eval.ok[categoryPlanFlip] {
		t.Fatal("one query's lookup failure must not fail the whole category")
	}
}

func TestPlanFlip_CheckEmitsTheFlip(t *testing.T) {
	db := &fakeFlipDB{rels: ordersStats(nil, flipAt(time.Hour)), now: flipT0.Add(3 * time.Hour)}
	a := flipAnalyzer(db)
	got := a.checkPlanFlips(context.Background(), flipPairs(t), true)
	if len(got) != 1 || got[0].Category != categoryPlanFlip ||
		got[0].Detail["relation"] != "public.orders" || got[0].RecommendedSQL == "" {
		t.Fatalf("findings = %+v, want one flip on public.orders", got)
	}
	cur, prev := got[0].Detail["current_plan_hash"], got[0].Detail["previous_plan_hash"]
	if cur == nil || prev == nil || cur == prev {
		t.Fatalf("hashes = %v / %v, want two different fingerprints", prev, cur)
	}
}

func TestPlanFlip_NoAnalyzeBetweenEmitsNothing(t *testing.T) {
	db := &fakeFlipDB{rels: ordersStats(nil, flipAt(-time.Hour)), now: flipT0.Add(3 * time.Hour)}
	a := flipAnalyzer(db)
	pairs := flipPairs(t)
	if got := a.checkPlanFlips(context.Background(), pairs, true); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
	if len(rulePlanRegression(pairs)) != 1 {
		t.Fatal("the plain regression rule must still fire on the pair")
	}
	if !a.eval.ok[categoryPlanFlip] {
		t.Fatal("the category was not marked evaluated")
	}
}

// Without the plan pairs nothing is known: the category fails, so its
// open findings stay open.
func TestPlanFlip_UnreadPairsFailTheCategory(t *testing.T) {
	a := flipAnalyzer(&fakeFlipDB{})
	if got := a.checkPlanFlips(context.Background(), nil, false); len(got) != 0 {
		t.Fatalf("findings = %+v", got)
	}
	if !a.eval.hasFailed(categoryPlanFlip) {
		t.Fatal("unread pairs did not fail the category")
	}
}

func TestPlanFlip_FailedReadsFailTheCategory(t *testing.T) {
	for name, db := range map[string]*fakeFlipDB{
		"own analyzes": {sageErr: errors.New("relation sage.action_log does not exist")},
		"open flips":   {openErr: errors.New("connection reset")},
	} {
		a := flipAnalyzer(db)
		a.checkPlanFlips(context.Background(), flipPairs(t), true)
		if !a.eval.hasFailed(categoryPlanFlip) {
			t.Errorf("%s: a failed read did not fail the category", name)
		}
	}
}
