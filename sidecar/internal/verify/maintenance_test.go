package verify

import "testing"

// earned-05: the VACUUM/ANALYZE judgement is shared by the executor (its
// own maintenance) and the shadow scorer (a maintenance run outside
// pg_sage), so both read the same bar.
// No concurrent access tests: JudgeMaintenanceMetric is a pure function.

func TestJudgeMaintenanceMetricVerdicts(t *testing.T) {
	cases := []struct {
		name          string
		class, metric string
		before, after float64
		want          string
	}{
		{"vacuum cleared", ClassVacuum, MetricDeadTuples, 1500, 0, OutcomeImproved},
		{"vacuum exactly half", ClassVacuum, MetricDeadTuples, 1000, 500, OutcomeImproved},
		{"vacuum just short of half", ClassVacuum, MetricDeadTuples, 1000, 501, OutcomeNeutral},
		{"vacuum nothing to do", ClassVacuum, MetricDeadTuples, 0, 0, OutcomeInsufficient},
		{"vacuum below one", ClassVacuum, MetricDeadTuples, 0.5, 0, OutcomeInsufficient},
		{"before unreadable", ClassVacuum, MetricDeadTuples, -1, 0, OutcomeUnverifiable},
		{"after unreadable", ClassAnalyze, MetricModsSinceAnalyze, 5000, -1,
			OutcomeUnverifiable},
		{"analyze reset", ClassAnalyze, MetricModsSinceAnalyze, 5000, 0, OutcomeImproved},
		{"analyze grew", ClassAnalyze, MetricModsSinceAnalyze, 5000, 9000, OutcomeNeutral},
		{"freeze advanced", ClassVacuum, MetricFrozenXIDAge, 2e8, 10, OutcomeImproved},
		{"not maintenance", ClassGUC, MetricDeadTuples, 10, 0, OutcomeUnverifiable},
		{"empty class", "", MetricDeadTuples, 10, 0, OutcomeUnverifiable},
	}
	for _, tc := range cases {
		got := JudgeMaintenanceMetric(tc.class, tc.metric, tc.before, tc.after)
		if got.Verdict != tc.want || got.Reason == "" || got.Metric != tc.metric ||
			got.Before != tc.before || got.After != tc.after {
			t.Errorf("%s: JudgeMaintenanceMetric = %+v, want %s", tc.name, got, tc.want)
		}
	}
}

func TestJudgeMaintenanceMetricObservedChange(t *testing.T) {
	got := JudgeMaintenanceMetric(ClassVacuum, MetricDeadTuples, 1000, 250)
	if got.ObservedPct == nil || *got.ObservedPct != -75 {
		t.Fatalf("observed change = %v, want -75%%", got.ObservedPct)
	}
	none := JudgeMaintenanceMetric(ClassVacuum, MetricDeadTuples, 0, 0)
	if none.ObservedPct != nil {
		t.Fatalf("no baseline must report no change, got %v", *none.ObservedPct)
	}
	if MaintenanceGainShare != 0.5 {
		t.Fatalf("gain share %v, want 0.5", MaintenanceGainShare)
	}
}
