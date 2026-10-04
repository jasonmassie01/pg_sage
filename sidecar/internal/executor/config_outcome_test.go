package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/verify"
)

// No concurrent access tests: the verdict functions are pure; the
// monitor's database path is covered by config_roundtrip_integration_test.

func TestOutcomeMetricFor(t *testing.T) {
	guc := func(sql string) *configChange {
		stmt, ok := pgconf.ParseAlterSystem(sql)
		if !ok {
			t.Fatalf("parse %q", sql)
		}
		return &configChange{kind: configKindGUC, guc: stmt}
	}
	rel := func(sql string) *configChange {
		stmt, ok := pgconf.ParseAlterTableReloptions(sql)
		if !ok {
			t.Fatalf("parse %q", sql)
		}
		return &configChange{kind: configKindReloption, table: stmt}
	}
	cases := []struct {
		change        *configChange
		metric, table string
	}{
		{guc("ALTER SYSTEM SET work_mem = '64MB'"), metricTempSpills, ""},
		{guc("ALTER SYSTEM SET autovacuum_vacuum_cost_limit = 2000"), metricDeadTuples, ""},
		{guc("ALTER SYSTEM SET autovacuum_vacuum_scale_factor = 0.05"), metricDeadTuples, ""},
		{guc("ALTER SYSTEM SET random_page_cost = 1.1"), "", ""},
		{guc("ALTER SYSTEM SET maintenance_work_mem = '1GB'"), "", ""},
		{rel(`ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.02)`),
			metricDeadTuples, `"public"."t"`},
		{rel(`ALTER TABLE public.t SET (toast.autovacuum_vacuum_threshold = 100)`),
			metricDeadTuples, "public.t"},
		{rel(`ALTER TABLE public.t SET (fillfactor = 90)`), metricHotUpdates, "public.t"},
		{rel(`ALTER TABLE public.t SET (autovacuum_analyze_threshold = 100)`), "", ""},
		{nil, "", ""},
	}
	for _, c := range cases {
		metric, table := outcomeMetricFor(c.change)
		if metric != c.metric || table != c.table {
			t.Errorf("outcomeMetricFor(%+v) = %q,%q want %q,%q", c.change, metric, table,
				c.metric, c.table)
		}
	}
}

// The judges return the Phase 1.3 four-way verdict instead of a bool:
// "credited" became improved, "did not move" neutral, "cannot tell"
// insufficient_evidence, and "no counters" unverifiable. Spills that at
// least doubled are a regression (the monitor rolls the change back).
func TestJudgeTempSpills(t *testing.T) {
	tenMin := 10 * time.Minute
	cases := []struct {
		name          string
		before, after map[string]float64
		elapsed       time.Duration
		verdict       string
		reason        string
	}{
		{"spills stopped", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1000}, tenMin, verify.OutcomeImproved, "fell"},
		// Expected 600 in 10 min; 300 observed is exactly half: improved.
		{"halved boundary", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1300}, tenMin, verify.OutcomeImproved, "fell"},
		{"just above half", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1301}, tenMin, verify.OutcomeNeutral,
			"did not fall"},
		{"doubled", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 2200}, tenMin, verify.OutcomeRegressed, "rose"},
		{"just below double", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 2199}, tenMin, verify.OutcomeNeutral,
			"did not fall"},
		{"no spills before", map[string]float64{"temp_files": 0, "rate_per_sec": 0},
			map[string]float64{"temp_files": 0}, tenMin, verify.OutcomeInsufficient,
			"no temp-file spills"},
		// 0.004/s * 600s = 2.4 expected spills: too few to tell.
		{"too few expected", map[string]float64{"temp_files": 50, "rate_per_sec": 0.004},
			map[string]float64{"temp_files": 50}, tenMin, verify.OutcomeInsufficient,
			"too few"},
		// 0.005/s * 600s = 3 expected: just enough to judge.
		{"three expected", map[string]float64{"temp_files": 50, "rate_per_sec": 0.005},
			map[string]float64{"temp_files": 50}, tenMin, verify.OutcomeImproved, "fell"},
		{"stats reset", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 10}, tenMin, verify.OutcomeInsufficient, "reset"},
		{"missing counter", map[string]float64{"rate_per_sec": 1},
			map[string]float64{"temp_files": 10}, tenMin, verify.OutcomeUnverifiable,
			"missing"},
		{"zero window", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1000}, 0, verify.OutcomeInsufficient, "too few"},
	}
	for _, c := range cases {
		got := judgeTempSpills(c.before, c.after, c.elapsed)
		if got.Verdict != c.verdict || !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: judgeTempSpills = %s,%q want %s,~%q", c.name, got.Verdict,
				got.Reason, c.verdict, c.reason)
		}
	}
}

func TestJudgeTempSpillsObservedChange(t *testing.T) {
	got := judgeTempSpills(map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
		map[string]float64{"temp_files": 1150}, 10*time.Minute)
	// 150 observed against 600 expected: -75%.
	if got.ObservedPct == nil || *got.ObservedPct < -75.01 || *got.ObservedPct > -74.99 {
		t.Fatalf("observed change = %v, want -75%%", got.ObservedPct)
	}
	if got.Before != 600 || got.After != 150 {
		t.Fatalf("before/after = %v/%v, want expected 600 vs observed 150", got.Before,
			got.After)
	}
}

func TestJudgeDeadTuples(t *testing.T) {
	b := map[string]float64{"n_dead_tup": 200, "n_live_tup": 800, "autovacuum_count": 3}
	cases := []struct {
		name    string
		before  map[string]float64
		after   map[string]float64
		verdict string
		reason  string
	}{
		{"vacuumed and fell", b,
			map[string]float64{"n_dead_tup": 10, "n_live_tup": 990, "autovacuum_count": 4},
			verify.OutcomeImproved, "fell"},
		// ratio 0.20 -> 0.16 is exactly 80%: improved boundary.
		{"80 percent boundary", b,
			map[string]float64{"n_dead_tup": 160, "n_live_tup": 840, "autovacuum_count": 4},
			verify.OutcomeImproved, "fell"},
		{"not enough", b,
			map[string]float64{"n_dead_tup": 170, "n_live_tup": 830, "autovacuum_count": 4},
			verify.OutcomeNeutral, "did not fall"},
		{"autovacuum idle", b,
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 1000, "autovacuum_count": 3},
			verify.OutcomeInsufficient, "did not run"},
		{"no dead tuples before",
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 1000, "autovacuum_count": 3},
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 1000, "autovacuum_count": 5},
			verify.OutcomeInsufficient, "no dead tuples"},
		{"empty table", map[string]float64{"n_dead_tup": 0, "n_live_tup": 0,
			"autovacuum_count": 0},
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 0, "autovacuum_count": 1},
			verify.OutcomeInsufficient, "no dead tuples"},
		{"missing counters", map[string]float64{}, map[string]float64{},
			verify.OutcomeUnverifiable, "missing"},
	}
	for _, c := range cases {
		got := judgeDeadTuples(c.before, c.after)
		if got.Verdict != c.verdict || !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: judgeDeadTuples = %s,%q want %s,~%q", c.name, got.Verdict,
				got.Reason, c.verdict, c.reason)
		}
	}
}

func TestJudgeHotUpdates(t *testing.T) {
	b := map[string]float64{"n_tup_upd": 1000, "n_tup_hot_upd": 500}
	cases := []struct {
		name    string
		after   map[string]float64
		verdict string
		reason  string
	}{
		{"hot ratio rose", map[string]float64{"n_tup_upd": 1200, "n_tup_hot_upd": 690},
			verify.OutcomeImproved, "rose"},
		// 0.50 lifetime -> 0.55 in window is exactly +0.05: improved.
		{"boundary", map[string]float64{"n_tup_upd": 1200, "n_tup_hot_upd": 610},
			verify.OutcomeImproved, "rose"},
		{"flat", map[string]float64{"n_tup_upd": 1200, "n_tup_hot_upd": 608},
			verify.OutcomeNeutral, "did not rise"},
		{"too few updates", map[string]float64{"n_tup_upd": 1099, "n_tup_hot_upd": 599},
			verify.OutcomeInsufficient, "too few"},
		{"reset", map[string]float64{"n_tup_upd": 10, "n_tup_hot_upd": 5},
			verify.OutcomeInsufficient, "too few"},
		{"missing", map[string]float64{}, verify.OutcomeUnverifiable, "missing"},
	}
	for _, c := range cases {
		got := judgeHotUpdates(b, c.after)
		if got.Verdict != c.verdict || !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: judgeHotUpdates = %s,%q want %s,~%q", c.name, got.Verdict,
				got.Reason, c.verdict, c.reason)
		}
	}
}

func TestJudgeOutcomeDispatch(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	got := judgeOutcome(outcomeBaseline{Metric: "nonsense", At: at}, nil, time.Now())
	if got.Verdict != verify.OutcomeUnverifiable ||
		!strings.Contains(got.Reason, "no targeted metric") {
		t.Errorf("unknown metric = %+v", got)
	}
	got = judgeOutcome(outcomeBaseline{Metric: metricTempSpills, At: at,
		Counters: map[string]float64{"temp_files": 1000, "rate_per_sec": 1}},
		map[string]float64{"temp_files": 1000}, time.Now())
	if got.Verdict != verify.OutcomeImproved || got.Metric != metricTempSpills {
		t.Errorf("temp spill dispatch = %+v, want improved temp_spills", got)
	}
}

func TestPredictionForConfigMetric(t *testing.T) {
	cases := map[string]float64{metricTempSpills: -50, metricDeadTuples: -20,
		metricHotUpdates: 5}
	for metric, want := range cases {
		p := configPrediction(metric)
		if p.Method != verify.MethodRule || p.Metric != metric ||
			p.ExpectedChangePct == nil || *p.ExpectedChangePct != want {
			t.Errorf("configPrediction(%s) = %+v, want rule %v", metric, p, want)
		}
	}
	if p := configPrediction(""); p.Predicts() || p.Method != verify.MethodNone {
		t.Errorf("configPrediction(none) = %+v, want no prediction", p)
	}
}
