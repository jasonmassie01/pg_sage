package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/pgconf"
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

func TestJudgeTempSpills(t *testing.T) {
	tenMin := 10 * time.Minute
	cases := []struct {
		name          string
		before, after map[string]float64
		elapsed       time.Duration
		ok            bool
		reason        string
	}{
		{"spills stopped", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1000}, tenMin, true, "fell"},
		// Expected 600 in 10 min; 300 observed is exactly half: success.
		{"halved boundary", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1300}, tenMin, true, "fell"},
		{"just above half", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1301}, tenMin, false, "did not fall"},
		{"no spills before", map[string]float64{"temp_files": 0, "rate_per_sec": 0},
			map[string]float64{"temp_files": 0}, tenMin, false, "no temp-file spills"},
		// 0.004/s * 600s = 2.4 expected spills: too few to tell.
		{"too few expected", map[string]float64{"temp_files": 50, "rate_per_sec": 0.004},
			map[string]float64{"temp_files": 50}, tenMin, false, "too few"},
		// 0.005/s * 600s = 3 expected: just enough to judge.
		{"three expected", map[string]float64{"temp_files": 50, "rate_per_sec": 0.005},
			map[string]float64{"temp_files": 50}, tenMin, true, "fell"},
		{"stats reset", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 10}, tenMin, false, "reset"},
		{"missing counter", map[string]float64{"rate_per_sec": 1},
			map[string]float64{"temp_files": 10}, tenMin, false, "missing"},
		{"zero window", map[string]float64{"temp_files": 1000, "rate_per_sec": 1},
			map[string]float64{"temp_files": 1000}, 0, false, "too few"},
	}
	for _, c := range cases {
		ok, reason := judgeTempSpills(c.before, c.after, c.elapsed)
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: judgeTempSpills = %v,%q want %v,~%q", c.name, ok, reason, c.ok, c.reason)
		}
	}
}

func TestJudgeDeadTuples(t *testing.T) {
	b := map[string]float64{"n_dead_tup": 200, "n_live_tup": 800, "autovacuum_count": 3}
	cases := []struct {
		name   string
		before map[string]float64
		after  map[string]float64
		ok     bool
		reason string
	}{
		{"vacuumed and fell", b,
			map[string]float64{"n_dead_tup": 10, "n_live_tup": 990, "autovacuum_count": 4},
			true, "fell"},
		// ratio 0.20 -> 0.16 is exactly 80%: success boundary.
		{"80 percent boundary", b,
			map[string]float64{"n_dead_tup": 160, "n_live_tup": 840, "autovacuum_count": 4},
			true, "fell"},
		{"not enough", b,
			map[string]float64{"n_dead_tup": 170, "n_live_tup": 830, "autovacuum_count": 4},
			false, "did not fall"},
		{"autovacuum idle", b,
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 1000, "autovacuum_count": 3},
			false, "did not run"},
		{"no dead tuples before",
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 1000, "autovacuum_count": 3},
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 1000, "autovacuum_count": 5},
			false, "no dead tuples"},
		{"empty table", map[string]float64{"n_dead_tup": 0, "n_live_tup": 0,
			"autovacuum_count": 0},
			map[string]float64{"n_dead_tup": 0, "n_live_tup": 0, "autovacuum_count": 1},
			false, "no dead tuples"},
		{"missing counters", map[string]float64{}, map[string]float64{}, false, "missing"},
	}
	for _, c := range cases {
		ok, reason := judgeDeadTuples(c.before, c.after)
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: judgeDeadTuples = %v,%q want %v,~%q", c.name, ok, reason, c.ok, c.reason)
		}
	}
}

func TestJudgeHotUpdates(t *testing.T) {
	b := map[string]float64{"n_tup_upd": 1000, "n_tup_hot_upd": 500}
	cases := []struct {
		name   string
		after  map[string]float64
		ok     bool
		reason string
	}{
		{"hot ratio rose", map[string]float64{"n_tup_upd": 1200, "n_tup_hot_upd": 690},
			true, "rose"},
		// 0.50 lifetime -> 0.55 in window is exactly +0.05: success.
		{"boundary", map[string]float64{"n_tup_upd": 1200, "n_tup_hot_upd": 610}, true, "rose"},
		{"flat", map[string]float64{"n_tup_upd": 1200, "n_tup_hot_upd": 608}, false,
			"did not rise"},
		{"too few updates", map[string]float64{"n_tup_upd": 1099, "n_tup_hot_upd": 599},
			false, "too few"},
		{"reset", map[string]float64{"n_tup_upd": 10, "n_tup_hot_upd": 5}, false, "too few"},
		{"missing", map[string]float64{}, false, "missing"},
	}
	for _, c := range cases {
		ok, reason := judgeHotUpdates(b, c.after)
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: judgeHotUpdates = %v,%q want %v,~%q", c.name, ok, reason, c.ok, c.reason)
		}
	}
}

func TestJudgeOutcomeDispatch(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	ok, reason := judgeOutcome(outcomeBaseline{Metric: "nonsense", At: at}, nil, time.Now())
	if ok || !strings.Contains(reason, "no targeted metric") {
		t.Errorf("unknown metric = %v,%q", ok, reason)
	}
	ok, _ = judgeOutcome(outcomeBaseline{Metric: metricTempSpills, At: at,
		Counters: map[string]float64{"temp_files": 1000, "rate_per_sec": 1}},
		map[string]float64{"temp_files": 1000}, time.Now())
	if !ok {
		t.Error("temp spill dispatch did not credit a stopped spill rate")
	}
}
