package querystore

import (
	"testing"
	"time"
)

// evid-01b without a database: when a day is due for the daily tier, how
// long the newest day is re-rolled, and the coverage status boundaries.
// No concurrent access tests: these are pure functions.

func TestDayDue_WaitsForTheLastHourAndTheQuarterPast(t *testing.T) {
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	end := day.Add(24 * time.Hour)
	last := day.Add(23 * time.Hour)
	for _, tc := range []struct {
		name     string
		now      time.Time
		through  time.Time
		wantDue  bool
		wantNote string
	}{
		{"23:00 rolled, 00:15", end.Add(15 * time.Minute), last, true, ""},
		{"23:00 rolled, 00:14:59", end.Add(15*time.Minute - time.Second), last, false, ""},
		{"22:00 rolled only", end.Add(time.Hour), last.Add(-time.Hour), false, ""},
		{"later hours rolled", end.Add(48 * time.Hour), end.Add(30 * time.Hour), true, ""},
	} {
		if got := dayDue(day, tc.now, tc.through); got != tc.wantDue {
			t.Errorf("%s: dayDue = %v, want %v", tc.name, got, tc.wantDue)
		}
	}
	if dayDue(day, end.Add(time.Hour), time.Time{}) {
		t.Error("no rolled hour (zero time): no day is due")
	}
}

func TestReRollNewest_UntilItsLastHourIsSettled(t *testing.T) {
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	end := day.Add(24 * time.Hour)
	// 23:00 is re-rolled as h-2 at 01:05; the day follows it until 01:15.
	if !reRollNewest(day, end.Add(75*time.Minute-time.Second)) {
		t.Error("01:14:59: the newest day must still be re-rolled")
	}
	if reRollNewest(day, end.Add(75*time.Minute)) {
		t.Error("01:15: the newest day is settled")
	}
	if reRollNewest(day, end.Add(48*time.Hour)) {
		t.Error("an old day is never re-rolled by the worker")
	}
}

func sums(over func(*coverageSums)) coverageSums {
	s := coverageSums{knownHours: 400, dbExecMs: 1000, sampledExecMs: 870,
		workloadHours: 400, breakHours: 0, span: 28 * 24 * time.Hour}
	over(&s)
	return s
}

func TestClassifyCoverage_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    coverageSums
		want string
	}{
		{"ok", sums(func(*coverageSums) {}), RollupCoverageOK},
		{"no known hour", sums(func(s *coverageSums) { s.knownHours, s.dbExecMs = 0, 0 }),
			RollupCoverageUnknown},
		{"no statement time", sums(func(s *coverageSums) { s.dbExecMs = 0 }),
			RollupCoverageUnknown},
		{"zero value", coverageSums{}, RollupCoverageUnknown},
		{"breaks at 25%", sums(func(s *coverageSums) { s.breakHours = 100 }),
			RollupCoverageOK},
		{"breaks at 25% + 1 hour", sums(func(s *coverageSums) { s.breakHours = 101 }),
			RollupCoverageMostlyBreaks},
		{"breaks with no workload hour", sums(func(s *coverageSums) {
			s.workloadHours, s.breakHours = 0, 3
		}), RollupCoverageOK},
		{"13 days 23 hours", sums(func(s *coverageSums) {
			s.span = 14*24*time.Hour - time.Hour
		}), RollupCoverageLearning},
		{"14 days", sums(func(s *coverageSums) { s.span = 14 * 24 * time.Hour }),
			RollupCoverageOK},
		{"short and mostly breaks", sums(func(s *coverageSums) {
			s.span, s.breakHours = time.Hour, 300
		}), RollupCoverageMostlyBreaks},
	} {
		if got := classifyCoverage(tc.s); got != tc.want {
			t.Errorf("%s: classifyCoverage = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCoveredShare_KnownHoursOnlyAndBounded(t *testing.T) {
	if got := coveredShare(coverageSums{}); got != nil {
		t.Fatalf("no known hour: share %v, want nil (unknown)", *got)
	}
	got := coveredShare(coverageSums{knownHours: 2, dbExecMs: 1000, sampledExecMs: 870})
	if got == nil || *got != 0.87 {
		t.Fatalf("share %v, want 0.87", got)
	}
	// The admitted set can include statements the totals leave out (pg_sage's
	// own): a share is never above the whole.
	if got := coveredShare(coverageSums{knownHours: 1, dbExecMs: 10,
		sampledExecMs: 12}); got == nil || *got != 1 {
		t.Fatalf("share %v, want 1", got)
	}
}
