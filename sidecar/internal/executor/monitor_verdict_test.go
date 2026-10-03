package executor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// No concurrent access tests: combineVerdict, the window arithmetic and
// the maintenance judges are pure functions.

func cmp(verdict string) *verify.Comparison {
	return &verify.Comparison{Verdict: verdict, Reason: "test " + verdict}
}

func rulePrediction(change float64) verify.Prediction {
	return verify.Prediction{Method: verify.MethodRule, ExpectedChangePct: &change}
}

func TestCombineVerdict(t *testing.T) {
	none := verify.NoPrediction(verify.ClassGUC, "advisor gave none")
	metric := func(v string) *metricJudgement { return &metricJudgement{Verdict: v} }
	cases := []struct {
		name    string
		class   string
		p       verify.Prediction
		queries *verify.Comparison
		metric  *metricJudgement
		want    string
	}{
		{"query regression wins", verify.ClassIndexCreate, rulePrediction(-30),
			cmp(verify.OutcomeRegressed), nil, verify.OutcomeRegressed},
		{"metric regression wins", verify.ClassGUC, rulePrediction(-50),
			cmp(verify.OutcomeImproved), metric(verify.OutcomeRegressed),
			verify.OutcomeRegressed},
		{"regression without prediction still counts", verify.ClassGUC, none,
			cmp(verify.OutcomeRegressed), nil, verify.OutcomeRegressed},
		{"no prediction is never credited", verify.ClassGUC, none,
			cmp(verify.OutcomeImproved), nil, verify.OutcomeUnverifiable},
		{"metric decides config", verify.ClassGUC, rulePrediction(-50),
			cmp(verify.OutcomeImproved), metric(verify.OutcomeNeutral), verify.OutcomeNeutral},
		{"metric insufficient", verify.ClassReloption, rulePrediction(-20), nil,
			metric(verify.OutcomeInsufficient), verify.OutcomeInsufficient},
		{"queries decide create", verify.ClassIndexCreate, rulePrediction(-30),
			cmp(verify.OutcomeImproved), nil, verify.OutcomeImproved},
		{"create neutral", verify.ClassIndexCreate, rulePrediction(-30),
			cmp(verify.OutcomeNeutral), nil, verify.OutcomeNeutral},
		{"drop held is improved", verify.ClassIndexDrop, rulePrediction(0),
			cmp(verify.OutcomeNeutral), nil, verify.OutcomeImproved},
		{"drop faster is improved", verify.ClassIndexDrop, rulePrediction(0),
			cmp(verify.OutcomeImproved), nil, verify.OutcomeImproved},
		{"drop without evidence", verify.ClassIndexDrop, rulePrediction(0),
			cmp(verify.OutcomeInsufficient), nil, verify.OutcomeInsufficient},
		{"drop without targets", verify.ClassIndexDrop, rulePrediction(0), nil, nil,
			verify.OutcomeInsufficient},
		{"nothing measurable", verify.ClassQueryHint, rulePrediction(-10), nil, nil,
			verify.OutcomeUnverifiable},
	}
	for _, tc := range cases {
		got, reason := combineVerdict(tc.class, tc.p, tc.queries, tc.metric)
		if got != tc.want {
			t.Errorf("%s: combineVerdict = %s (%s), want %s", tc.name, got, reason, tc.want)
		}
		if reason == "" {
			t.Errorf("%s: verdict without a reason", tc.name)
		}
	}
}

func testMonitorConfig() RollbackMonitorConfig {
	return RollbackMonitorConfig{ThresholdPct: 10, WindowMinutes: 15, CapMinutes: 4320,
		DropWindow: 168 * time.Hour}
}

func TestMonitorWindows(t *testing.T) {
	cfg := testMonitorConfig()
	minW, capW, every := monitorWindows(verify.ClassIndexDrop, cfg)
	if minW != 168*time.Hour || capW != 168*time.Hour || every != time.Hour {
		t.Fatalf("drop windows = %s/%s/%s, want 168h/168h/1h", minW, capW, every)
	}
	minW, capW, every = monitorWindows(verify.ClassGUC, cfg)
	if minW != 15*time.Minute || capW != 72*time.Hour || every != 5*time.Minute {
		t.Fatalf("guc windows = %s/%s/%s, want 15m/72h/5m", minW, capW, every)
	}
	fast := cfg
	fast.DropWindow = 2 * time.Hour
	minW, capW, every = monitorWindows(verify.ClassIndexDrop, fast)
	if minW != 2*time.Hour || capW != 2*time.Hour || every != 15*time.Minute {
		t.Fatalf("fast drop windows = %s/%s/%s, want 2h/2h/15m", minW, capW, every)
	}
}

func TestMonitorWindowsZeroConfigUsesSafeDefaults(t *testing.T) {
	minW, capW, every := monitorWindows(verify.ClassIndexDrop, RollbackMonitorConfig{})
	if minW != 168*time.Hour || capW != minW || every <= 0 {
		t.Fatalf("unset drop window = %s/%s/%s, want the 7-day business cycle", minW, capW,
			every)
	}
	minW, capW, every = monitorWindows(verify.ClassIndexCreate, RollbackMonitorConfig{})
	if minW != 0 || capW != 0 || every <= 0 {
		t.Fatalf("unset create windows = %s/%s/%s, want an immediate single check", minW,
			capW, every)
	}
}

func TestMonitorPlanSchedule(t *testing.T) {
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	drop := monitorPlan{class: verify.ClassIndexDrop, executedAt: at, softDrop: true,
		minWindow: 168 * time.Hour, capWindow: 168 * time.Hour, checkEvery: time.Hour}
	if got := drop.firstCheck(); !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("soft-drop first check = %s, want +1h (watch from the start)", got)
	}
	guc := monitorPlan{class: verify.ClassGUC, executedAt: at, minWindow: 15 * time.Minute,
		capWindow: 72 * time.Hour, checkEvery: 5 * time.Minute}
	if got := guc.firstCheck(); !got.Equal(at.Add(15 * time.Minute)) {
		t.Fatalf("guc first check = %s, want the minimum window", got)
	}
	last := at.Add(72*time.Hour - time.Minute)
	if got := guc.nextCheck(last); !got.Equal(at.Add(72 * time.Hour)) {
		t.Fatalf("next check = %s, want capped at the hard window", got)
	}
}

func TestMonitorPlanFinal(t *testing.T) {
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	p := monitorPlan{executedAt: at, minWindow: time.Hour, capWindow: 10 * time.Hour}
	cases := []struct {
		name    string
		now     time.Time
		verdict string
		want    bool
	}{
		{"before min, decided", at.Add(30 * time.Minute), verify.OutcomeImproved, false},
		{"after min, decided", at.Add(time.Hour), verify.OutcomeNeutral, true},
		{"after min, insufficient", at.Add(2 * time.Hour), verify.OutcomeInsufficient, false},
		{"at cap, insufficient", at.Add(10 * time.Hour), verify.OutcomeInsufficient, true},
		{"unverifiable is final", at.Add(time.Minute), verify.OutcomeUnverifiable, true},
		{"regressed is final", at.Add(time.Minute), verify.OutcomeRegressed, true},
	}
	for _, tc := range cases {
		if got := p.final(tc.now, tc.verdict); got != tc.want {
			t.Errorf("%s: final = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestJudgeMaintenance(t *testing.T) {
	cases := []struct {
		name          string
		class         string
		before, after float64
		want          string
	}{
		{"vacuum cleared", verify.ClassVacuum, 1500, 0, verify.OutcomeImproved},
		{"vacuum half", verify.ClassVacuum, 1000, 500, verify.OutcomeImproved},
		{"vacuum held back", verify.ClassVacuum, 1000, 900, verify.OutcomeNeutral},
		{"vacuum nothing to do", verify.ClassVacuum, 0, 0, verify.OutcomeInsufficient},
		{"vacuum unreadable", verify.ClassVacuum, -1, 0, verify.OutcomeUnverifiable},
		{"analyze reset mods", verify.ClassAnalyze, 5000, 0, verify.OutcomeImproved},
		{"analyze no change", verify.ClassAnalyze, 5000, 5000, verify.OutcomeNeutral},
		{"other class", verify.ClassGUC, 10, 0, verify.OutcomeUnverifiable},
	}
	for _, tc := range cases {
		got := judgeMaintenance(tc.class, tc.before, tc.after)
		if got.Verdict != tc.want || got.Reason == "" {
			t.Errorf("%s: judgeMaintenance = %+v, want %s", tc.name, got, tc.want)
		}
	}
	got := judgeMaintenance(verify.ClassVacuum, 1000, 250)
	if got.ObservedPct == nil || *got.ObservedPct != -75 {
		t.Fatalf("observed change = %v, want -75%%", got.ObservedPct)
	}
}

func TestRetentionVerdict(t *testing.T) {
	errOutside := errors.New("rows outside the predicate")
	cases := []struct {
		name       string
		deleted    int64
		candidates int64
		err        error
		want       string
	}{
		{"deleted all", 500, 500, nil, verify.OutcomeImproved},
		{"deleted bounded share", 100, 500, nil, verify.OutcomeImproved},
		{"deleted nothing", 0, 500, nil, verify.OutcomeNeutral},
		{"no candidates", 0, 0, nil, verify.OutcomeInsufficient},
		{"verification failed", 500, 500, errOutside, verify.OutcomeRegressed},
	}
	for _, tc := range cases {
		got := retentionVerdict(tc.deleted, tc.candidates, tc.err)
		if got.Verdict != tc.want {
			t.Errorf("%s: retentionVerdict = %+v, want %s", tc.name, got, tc.want)
		}
	}
	got := retentionVerdict(100, 500, nil)
	if got.ObservedPct == nil || *got.ObservedPct != -20 || !strings.Contains(got.Reason, "100") {
		t.Fatalf("retention observed = %+v, want -20%% of candidates", got)
	}
}

func TestLifecycleMapping(t *testing.T) {
	cases := map[string][2]string{
		verify.OutcomeImproved:     {"success", "success"},
		verify.OutcomeNeutral:      {"success", "unverifiable"},
		verify.OutcomeInsufficient: {"unverifiable", "unverifiable"},
		verify.OutcomeUnverifiable: {"unverifiable", "unverifiable"},
	}
	for verdict, want := range cases {
		if got := lifecycleOutcome(verdict); got != want[0] {
			t.Errorf("lifecycleOutcome(%s) = %s, want %s", verdict, got, want[0])
		}
		if got := verificationVerdictFor(verdict); got != want[1] {
			t.Errorf("verificationVerdictFor(%s) = %s, want %s (only improved is credited)",
				verdict, got, want[1])
		}
	}
	if got := verificationVerdictFor(verify.OutcomeRegressed); got != "revert" {
		t.Errorf("verificationVerdictFor(regressed) = %s, want revert", got)
	}
}
