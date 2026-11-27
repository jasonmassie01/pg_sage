package forecaster

import (
	"math"
	"reflect"
	"testing"
	"time"
)

// forecast-08: the daily rules read closed calendar days only. Today's
// partial day is left out, a day the collector watched for less than 80%
// of its hours is uncovered, and a missing day is marked rather than
// silently closed up.

// No concurrent access tests here: closedDays and the closed-day
// conversions are pure functions of their arguments;
// connection_limit_db_test.go runs two Forecast calls at once.

// calToday is the session's current day start in these fixtures.
var calToday = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

// fullRow is a day `offset` days before calToday watched every interval.
func fullRow(offset int, interval time.Duration) dayRow {
	day := calToday.AddDate(0, 0, -offset)
	n := int64(24 * time.Hour / interval)
	return dayRow{Day: day, Samples: n, FirstAt: day,
		LastAt: day.Add(time.Duration(n-1) * interval)}
}

// fullRows is offsets oldest..1 of fullRow, oldest first.
func fullRows(oldest int, interval time.Duration) []dayRow {
	rows := make([]dayRow, 0, oldest)
	for off := oldest; off >= 1; off-- {
		rows = append(rows, fullRow(off, interval))
	}
	return rows
}

func TestClosedDays_TodayIsNeverClosed(t *testing.T) {
	rows := append(fullRows(3, time.Minute), dayRow{Day: calToday, Samples: 1,
		FirstAt: calToday, LastAt: calToday})
	days := closedDays(rows, calToday, time.Minute)
	if len(days) != 3 {
		t.Fatalf("closed days = %d, want 3 (today is partial): %+v", len(days), days)
	}
	for i, d := range days {
		if d.Offset != 3-i || !d.Covered || d.Row != i {
			t.Errorf("day %d = %+v, want offset %d, covered, row %d", i, d, 3-i, i)
		}
	}
}

// Boundary: 79% of a day is not enough, 80% is.
func TestClosedDays_CoverageBoundary79And80Percent(t *testing.T) {
	rows := fullRows(14, time.Minute)
	rows[3].Samples = 1152 // 1152 min / 1440 min = 80.0%
	rows[5].Samples = 1137 // 78.96%
	rows[7].Samples = 1138 // 79.03%
	days := closedDays(rows, calToday, time.Minute)
	if len(days) != 14 {
		t.Fatalf("closed days = %d, want 14", len(days))
	}
	if !days[3].Covered || math.Abs(days[3].Coverage-0.8) > 1e-9 {
		t.Errorf("80%% day = %+v, want covered at 0.8", days[3])
	}
	for _, i := range []int{5, 7} {
		if days[i].Covered {
			t.Errorf("day %d at %.4f coverage is covered, want excluded", i, days[i].Coverage)
		}
	}
	for _, i := range []int{0, 1, 2, 4, 6, 8, 13} {
		if !days[i].Covered || days[i].Coverage < 0.999 {
			t.Errorf("full day %d = %+v, want covered", i, days[i])
		}
	}
}

// A missing day is in the calendar, marked, with no row.
func TestClosedDays_MissingDayIsMarked(t *testing.T) {
	rows := fullRows(14, time.Hour)
	rows = append(rows[:9:9], rows[10:]...) // offset 5 has no snapshot
	days := closedDays(rows, calToday, time.Hour)
	if len(days) != 14 {
		t.Fatalf("calendar = %d days, want 14 (the missing day is marked)", len(days))
	}
	miss := days[9]
	if miss.Offset != 5 || miss.Row != -1 || miss.Covered || miss.Coverage != 0 {
		t.Fatalf("missing day = %+v, want offset 5, no row, uncovered", miss)
	}
	if !miss.Day.Equal(calToday.AddDate(0, 0, -5)) {
		t.Errorf("missing day start = %v, want %v", miss.Day, calToday.AddDate(0, 0, -5))
	}
	if days[10].Row != 9 || days[10].Offset != 4 {
		t.Errorf("day after the gap = %+v, want row 9 at offset 4", days[10])
	}
}

// A burst of samples in one hour is not a covered day: the coverage is
// capped by the hours between the day's first and last sample.
func TestClosedDays_BurstDoesNotCoverADay(t *testing.T) {
	rows := fullRows(8, time.Minute)
	day := rows[2].Day
	rows[2] = dayRow{Day: day, Samples: 1440, FirstAt: day.Add(10 * time.Hour),
		LastAt: day.Add(11 * time.Hour)}
	days := closedDays(rows, calToday, time.Minute)
	if days[2].Covered || days[2].Coverage > 0.05 {
		t.Fatalf("one-hour burst = %+v, want uncovered near 1/24", days[2])
	}
}

// The coverage interval is the configured collector interval or the
// window's median sample spacing, whichever is longer: a raised
// collector.interval_seconds does not uncover every day until a restart.
func TestClosedDays_IntervalIsAtLeastTheMedianSpacing(t *testing.T) {
	rows := fullRows(10, 5*time.Minute) // collected every 5 min
	days := closedDays(rows, calToday, time.Minute)
	for i, d := range days {
		if !d.Covered {
			t.Fatalf("day %d uncovered at %.3f: the 5 min spacing was not used", i, d.Coverage)
		}
	}
}

// Nil/empty/zero: no rows is no closed day; a zero interval does not
// measure coverage (callers without a collector, one-off reads).
func TestClosedDays_EmptyAndZeroInterval(t *testing.T) {
	if got := closedDays(nil, calToday, time.Minute); len(got) != 0 {
		t.Fatalf("nil rows = %+v, want none", got)
	}
	only := []dayRow{{Day: calToday, Samples: 5, FirstAt: calToday, LastAt: calToday}}
	if got := closedDays(only, calToday, time.Minute); len(got) != 0 {
		t.Fatalf("today only = %+v, want none", got)
	}
	sparse := []dayRow{{Day: calToday.AddDate(0, 0, -2), Samples: 1},
		{Day: calToday.AddDate(0, 0, -1), Samples: 2}}
	got := closedDays(sparse, calToday, 0)
	if len(got) != 2 || !got[0].Covered || !got[1].Covered {
		t.Fatalf("zero interval = %+v, want both days covered (coverage not measured)", got)
	}
}

// Invalid input: rows dated after today (a clock step, a stale session
// day) are not closed days.
func TestClosedDays_FutureRowsAreDropped(t *testing.T) {
	rows := append(fullRows(2, time.Hour), fullRow(-1, time.Hour))
	if got := closedDays(rows, calToday, time.Hour); len(got) != 2 {
		t.Fatalf("closed days = %+v, want the 2 past days only", got)
	}
}

// --- system days ---

// calSystemRows is 14 closed days, offsets 14..1, collected hourly, that
// give a finding in all four system categories, then today's row.
func calSystemRows(today DaySystemAgg) []DaySystemAgg {
	rows := make([]DaySystemAgg, 0, 15)
	for i := 0; i < 14; i++ {
		day := calToday.AddDate(0, 0, i-14)
		size := 100e9 + float64(i)*10e9
		rows = append(rows, DaySystemAgg{Day: day, Samples: 24, FirstAt: day,
			LastAt: day.Add(23 * time.Hour), AvgDBSizeBytes: size, MaxDBSizeBytes: size,
			MaxActiveBackends: 10, MaxTotalBackends: float64(50 + 3*i),
			MaxClientBackends: float64(40 + 3*i), MaxReservedConnections: 3,
			HasClientCounts: true,
			MaxConnections:  200, CacheHitRatio: 0.8, CacheBlocksAccessed: 1e6,
			CheckpointsTimed: float64(288 * i), CheckpointsRequested: float64(120 * i),
			CheckpointsLastAt: day.Add(23 * time.Hour)})
	}
	return append(rows, today)
}

// calSystemConfig warns on all four system categories of calSystemRows.
func calSystemConfig() ForecasterConfig {
	return ForecasterConfig{LookbackDays: 30, DiskWarnGrowthGBDay: 5, ConnectionWarnPct: 80,
		CacheWarnThreshold: 0.95, MinRSquared: 0.5, CollectorInterval: time.Hour}
}

// State transition (CHECK-25): the same history evaluated at 00:10 and at
// 23:50 gives identical findings for all four system categories.
func TestCalendarParity_SystemCategoriesAt0010And2350(t *testing.T) {
	early := DaySystemAgg{Day: calToday, Samples: 1, FirstAt: calToday, LastAt: calToday,
		AvgDBSizeBytes: 900e9, MaxDBSizeBytes: 900e9, MaxTotalBackends: 199,
		MaxClientBackends: 199, MaxConnections: 200, CacheHitRatio: 0.1,
		CacheBlocksAccessed: 5e6}
	late := DaySystemAgg{Day: calToday, Samples: 143, FirstAt: calToday,
		LastAt: calToday.Add(23*time.Hour + 50*time.Minute), AvgDBSizeBytes: 50e9,
		MaxDBSizeBytes: 50e9, MaxTotalBackends: 2, MaxClientBackends: 1,
		MaxConnections: 200, CacheHitRatio: 1, CacheBlocksAccessed: 9e9,
		CheckpointsTimed: 1e6, CheckpointsRequested: 1e6,
		CheckpointsLastAt: calToday.Add(23 * time.Hour)}
	cfg := calSystemConfig()
	at0010 := systemFindings(closedSystemDays(calSystemRows(early), calToday, time.Hour), cfg)
	at2350 := systemFindings(closedSystemDays(calSystemRows(late), calToday, time.Hour), cfg)
	if !reflect.DeepEqual(at0010, at2350) {
		t.Fatalf("findings differ by the hour:\n00:10 %+v\n23:50 %+v", at0010, at2350)
	}
	for name, got := range map[string]int{"disk": len(at0010.disk),
		"connection": len(at0010.connection), "cache": len(at0010.cache),
		"checkpoint": len(at0010.checkpoint)} {
		if got == 0 {
			t.Errorf("no %s finding: the parity would hold vacuously", name)
		}
	}
	if len(at0010.evaluated) != len(systemForecastCategories) {
		t.Errorf("evaluated = %v, want all four system categories", at0010.evaluated)
	}
	if cur := at0010.disk[0].Detail["current_size_gb"].(float64); math.Abs(cur-230) > 1e-6 {
		t.Errorf("current size = %v GB, want yesterday's 230", cur)
	}
}

// An uncovered day keeps its place but reads as unknown everywhere, so
// forecast-07's unknown-day handling applies to it.
func TestClosedSystemDays_UncoveredDayIsUnknown(t *testing.T) {
	rows := calSystemRows(DaySystemAgg{Day: calToday, Samples: 1})
	rows[4].Samples = 6 // 6 of 24 hours
	got := closedSystemDays(rows, calToday, time.Hour)
	if len(got) != 14 {
		t.Fatalf("closed days = %d, want 14", len(got))
	}
	u := got[4]
	if !u.DBSizeUnknown || !u.BackendsUnknown || !u.MaxConnectionsUnknown ||
		!u.CacheUnknown || !u.CheckpointsUnknown || u.HasClientCounts {
		t.Fatalf("uncovered day = %+v, want every input unknown", u)
	}
	if u.MaxDBSizeBytes != 0 || u.MaxTotalBackends != 0 || u.CacheHitRatio != 0 {
		t.Errorf("uncovered day keeps values: %+v", u)
	}
	if got[5].DBSizeUnknown || got[5].MaxDBSizeBytes == 0 {
		t.Errorf("covered neighbour changed: %+v", got[5])
	}
	if rows[4].DBSizeUnknown {
		t.Error("closedSystemDays modified its input")
	}
}

// --- query days ---

// calQueryRows is `days` closed days of calls(i) collected hourly, oldest
// first, then today's row.
func calQueryRows(days int, calls func(i int) float64, today DayQueryAgg) []DayQueryAgg {
	rows := make([]DayQueryAgg, 0, days+1)
	for i := 0; i < days; i++ {
		day := calToday.AddDate(0, 0, i-days)
		rows = append(rows, DayQueryAgg{Day: day, TotalCalls: calls(i), Samples: 24,
			FirstAt: day, LastAt: day.Add(23 * time.Hour)})
	}
	return append(rows, today)
}

// State transition (CHECK-25): query volume at 00:10 and 23:50.
func TestCalendarParity_QueryVolumeAt0010And2350(t *testing.T) {
	calls := func(i int) float64 {
		if i < 8 {
			return 1000
		}
		return 1600
	}
	early := DayQueryAgg{Day: calToday, TotalCalls: 5, Samples: 1, FirstAt: calToday,
		LastAt: calToday}
	late := DayQueryAgg{Day: calToday, TotalCalls: 90000, Samples: 143, FirstAt: calToday,
		LastAt: calToday.Add(23*time.Hour + 50*time.Minute)}
	cfg := calSystemConfig()
	a := forecastQueryVolume(closedQueryDays(calQueryRows(15, calls, early), calToday,
		time.Hour), cfg)
	b := forecastQueryVolume(closedQueryDays(calQueryRows(15, calls, late), calToday,
		time.Hour), cfg)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("query volume differs by the hour:\n00:10 %+v\n23:50 %+v", a, b)
	}
	if len(a) != 1 || a[0].Severity != "warning" {
		t.Fatalf("findings = %+v, want one 60%%/week warning", a)
	}
	if g := a[0].Detail["wow_growth_pct"].(float64); math.Abs(g-60) > 1e-9 {
		t.Errorf("wow = %v, want 60 over the two weeks ending yesterday", g)
	}
}

// Boundary: 14 days with day 9 missing gives "not evaluated" for WoW.
func TestCalendarWoW_MissingDayNineIsNotEvaluated(t *testing.T) {
	rows := calQueryRows(14, func(i int) float64 { return 1000 + float64(i)*200 },
		DayQueryAgg{Day: calToday, Samples: 1})
	rows = append(rows[:9:9], rows[10:]...)
	days := closedQueryDays(rows, calToday, time.Hour)
	if len(days) != 14 || !days[9].Uncovered {
		t.Fatalf("closed query days = %+v, want 14 with day 9 uncovered", days)
	}
	if _, ok := weekOverWeek(days); ok {
		t.Fatal("WoW evaluated across a missing day")
	}
	if f := forecastQueryVolume(days, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("finding across a missing day: %+v", f)
	}
}

// Nil/empty: no rows is no finding and the category is not evaluated.
func TestCalendarWoW_NoRowsNotEvaluated(t *testing.T) {
	days := closedQueryDays(nil, calToday, time.Hour)
	if _, ok := weekOverWeek(days); ok {
		t.Fatal("WoW evaluated without history")
	}
	if f := forecastQueryVolume(days, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("finding without history: %+v", f)
	}
}

// Boundary: the two weeks must end yesterday. With yesterday uncovered
// the newest 14 days are not two complete weeks.
func TestCalendarWoW_YesterdayUncoveredNotEvaluated(t *testing.T) {
	rows := calQueryRows(20, func(int) float64 { return 1000 }, DayQueryAgg{Day: calToday})
	rows[19].Samples = 3
	if _, ok := weekOverWeek(closedQueryDays(rows, calToday, time.Hour)); ok {
		t.Fatal("WoW evaluated with yesterday uncovered")
	}
	rows[19].Samples = 24
	w, ok := weekOverWeek(closedQueryDays(rows, calToday, time.Hour))
	if !ok || w.pct != 0 || w.thisWeek != 7000 || w.lastWeek != 7000 {
		t.Fatalf("flat weeks = %+v (%v), want 0%% over 7000 and 7000", w, ok)
	}
	if !w.to.Equal(calToday.AddDate(0, 0, -1)) || !w.from.Equal(calToday.AddDate(0, 0, -14)) {
		t.Errorf("window %v..%v, want the 14 days ending yesterday", w.from, w.to)
	}
}

// Invalid input: a prior week without calls has no growth rate.
func TestCalendarWoW_ZeroPriorWeekNotEvaluated(t *testing.T) {
	rows := calQueryRows(14, func(i int) float64 {
		if i < 7 {
			return 0
		}
		return 500
	}, DayQueryAgg{Day: calToday})
	if w, ok := weekOverWeek(closedQueryDays(rows, calToday, time.Hour)); ok {
		t.Fatalf("WoW = %+v over a zero prior week, want not evaluated", w)
	}
}

// A delta that spans a collector gap is divided by its hours: the day
// after an 18-hour outage keeps only the share of the calls that fell
// inside it, and the day before gets its own share back.
func TestClosedQueryDays_GapDeltaIsSharedByHours(t *testing.T) {
	rows := calQueryRows(3, func(int) float64 { return 2400 }, DayQueryAgg{Day: calToday})
	d1, d2 := rows[1].Day, rows[2].Day
	// The collector was down from 20:00 on day 1 to 01:00 on day 2; both
	// days stay above 80% coverage.
	rows[1].LastAt = d1.Add(20 * time.Hour)
	rows[1].Samples = 21
	rows[1].TotalCalls = 2000 // 00:00..20:00 at 100/h
	rows[2].FirstAt = d2.Add(1 * time.Hour)
	rows[2].Samples = 23
	// Day 2's first sample counted 500 calls since day 1's last (5 h at
	// 100/h): 400 of them belong to day 1, 100 to day 2.
	rows[2].TotalCalls = 500 + 2200
	rows[2].CarryCalls = 500
	rows[2].CarryFrom, rows[2].CarryTo = rows[1].LastAt, rows[2].FirstAt
	rows[2].CarryFromDay = d1
	days := closedQueryDays(rows, calToday, time.Hour)
	if len(days) != 3 {
		t.Fatalf("days = %+v", days)
	}
	if math.Abs(days[1].TotalCalls-2400) > 1e-9 || math.Abs(days[2].TotalCalls-2300) > 1e-9 {
		t.Fatalf("totals = %v / %v, want 2400 / 2300 (the 5 h delta shared by hour)",
			days[1].TotalCalls, days[2].TotalCalls)
	}
	if rows[2].TotalCalls != 2700 {
		t.Error("closedQueryDays modified its input")
	}
}

// A day after a missing day keeps only its own share of the long delta.
func TestClosedQueryDays_DeltaAcrossMissingDay(t *testing.T) {
	rows := calQueryRows(4, func(int) float64 { return 2400 }, DayQueryAgg{Day: calToday})
	last := rows[0].Day.Add(23 * time.Hour)
	first := rows[2].Day.Add(time.Hour)
	rows[2].FirstAt = first
	rows[2].CarryCalls = 2600 // 26 h at 100/h, 1 h of it on day 2
	rows[2].TotalCalls = 2600 + 2200
	rows[2].CarryFrom, rows[2].CarryTo, rows[2].CarryFromDay = last, first, rows[0].Day
	rows = append(rows[:1:1], rows[2:]...) // day 1 has no snapshot
	days := closedQueryDays(rows, calToday, time.Hour)
	if len(days) != 4 || !days[1].Uncovered {
		t.Fatalf("days = %+v, want 4 with the missing day uncovered", days)
	}
	if math.Abs(days[2].TotalCalls-2300) > 1e-9 {
		t.Fatalf("day after the gap = %v, want 2300 (2200 + 1 h of the 26 h delta)",
			days[2].TotalCalls)
	}
	if math.Abs(days[0].TotalCalls-2500) > 1e-9 {
		t.Errorf("day before the gap = %v, want 2500 (its last hour back)", days[0].TotalCalls)
	}
}

// Today's first sample counts the calls since yesterday's last: yesterday
// gets its share although today is not a closed day.
func TestClosedQueryDays_TodaysFirstSampleGivesYesterdayItsTail(t *testing.T) {
	y := calToday.AddDate(0, 0, -1)
	today := DayQueryAgg{Day: calToday, TotalCalls: 150, Samples: 1,
		FirstAt: calToday.Add(30 * time.Minute), LastAt: calToday.Add(30 * time.Minute),
		CarryCalls: 150, CarryFrom: y.Add(23 * time.Hour),
		CarryTo: calToday.Add(30 * time.Minute), CarryFromDay: y}
	rows := calQueryRows(2, func(int) float64 { return 2300 }, today)
	days := closedQueryDays(rows, calToday, time.Hour)
	if len(days) != 2 {
		t.Fatalf("days = %+v, want the 2 closed days", days)
	}
	if math.Abs(days[1].TotalCalls-2400) > 1e-9 {
		t.Fatalf("yesterday = %v, want 2400 (its last hour from today's first sample)",
			days[1].TotalCalls)
	}
	if days[0].TotalCalls != 2300 {
		t.Errorf("the day before = %v, want 2300 unchanged", days[0].TotalCalls)
	}
}
