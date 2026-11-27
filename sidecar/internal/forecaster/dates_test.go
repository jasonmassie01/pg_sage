package forecaster

import (
	"strings"
	"testing"
	"time"
)

// forecast-08 review: the dates in titles and details are the session's
// calendar dates. pgx returns a timestamptz in Go's Local zone, so a
// session-midnight day start reads as the previous evening west of the
// session; formatting that instant named the day before.

// No concurrent access tests: these are pure functions of their arguments.

// west is a Go zone six hours behind a UTC session (a sidecar in Chicago).
var west = time.FixedZone("west", -6*3600)

// sessionMidnights returns n consecutive UTC session day starts ending on
// 2026-11-10, as pgx hands them to a sidecar in the west zone.
func sessionMidnights(n int) []time.Time {
	end := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	out := make([]time.Time, n)
	for i := range out {
		out[i] = end.AddDate(0, 0, i-n+1).In(west)
	}
	return out
}

func TestCalendarDate_IsTheSessionDate(t *testing.T) {
	utcMidnight := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		day  time.Time
	}{
		{"same zone", utcMidnight},
		{"Go zone west of the session", utcMidnight.In(west)},
		{"Go zone east of the session", utcMidnight.In(time.FixedZone("e", 5*3600))},
		{"DST hour early", utcMidnight.Add(-time.Hour)},
		{"DST hour late", utcMidnight.Add(time.Hour)},
	}
	for _, c := range cases {
		if got := calendarDate(c.day).Format(dateLayout); got != "2026-11-10" {
			t.Errorf("%s: calendarDate(%v) = %s, want 2026-11-10", c.name, c.day, got)
		}
	}
}

func TestProjectedDate_Bounds(t *testing.T) {
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).In(west)
	cases := []struct {
		days float64
		want string
		ok   bool
	}{
		{0, "2026-11-10", true},
		{17, "2026-11-27", true},
		{28.9, "2026-12-08", true}, // whole days: the threshold falls on 12-08
		{maxProjectedDays, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).
			AddDate(0, 0, maxProjectedDays).Format(dateLayout), true},
		{maxProjectedDays + 1, "", false},
		{1e6, "", false}, // past time.Duration's range: no 1700s date
		{-1, "", false},
	}
	for _, c := range cases {
		got, ok := projectedDate(day, c.days)
		if got != c.want || ok != c.ok {
			t.Errorf("projectedDate(%v) = %q, %v; want %q, %v", c.days, got, ok, c.want, c.ok)
		}
	}
}

// The connection title's date counts from the session date of the newest
// day: 126 of a 160 limit at 2 a day is 17 days after 2026-11-10.
func TestConnectionTitle_ProjectedDateIsTheSessionDate(t *testing.T) {
	days := sessionMidnights(14)
	aggs := make([]DaySystemAgg, len(days))
	for i, d := range days {
		aggs[i] = DaySystemAgg{Day: d, MaxTotalBackends: float64(100 + 2*i), MaxConnections: 200}
	}
	f := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(f) != 1 {
		t.Fatalf("findings = %d, want 1", len(f))
	}
	if f[0].Detail["projected_date"] != "2026-11-27" {
		t.Errorf("projected_date = %v, want 2026-11-27", f[0].Detail["projected_date"])
	}
	if !strings.Contains(f[0].Title, "by 2026-11-27") {
		t.Errorf("title %q lacks 2026-11-27", f[0].Title)
	}
}

// The query volume window names the session dates of its first and last
// day: 2026-10-28 to 2026-11-10.
func TestQueryVolumeWindow_IsTheSessionDates(t *testing.T) {
	days := sessionMidnights(14)
	q := make([]DayQueryAgg, len(days))
	for i, d := range days {
		q[i] = DayQueryAgg{Day: d, TotalCalls: 100}
		if i >= 7 {
			q[i].TotalCalls = 200
		}
	}
	f := forecastQueryVolume(q, ForecasterConfig{})
	if len(f) != 1 {
		t.Fatalf("findings = %d, want 1 (100%%/week)", len(f))
	}
	d := f[0].Detail
	if d["window_start"] != "2026-10-28" || d["window_end"] != "2026-11-10" {
		t.Errorf("window = %v..%v, want 2026-10-28..2026-11-10",
			d["window_start"], d["window_end"])
	}
	if !strings.Contains(f[0].Title, "week to 2026-11-10") {
		t.Errorf("title %q lacks the week's end 2026-11-10", f[0].Title)
	}
}

// The disk title's full date counts from the newest day's session date,
// and a runway beyond maxProjectedDays carries no date at all.
func TestProjectedFull_SessionDateAndHugeRunway(t *testing.T) {
	asOf := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).In(west)
	near := map[string]any{"days_until_full": 30.0, "as_of_day": asOf}
	if got := projectedFull(near); got != ", full by 2026-12-10" {
		t.Errorf("projectedFull(30 days) = %q, want \", full by 2026-12-10\"", got)
	}
	if near["projected_full_date"] != "2026-12-10" {
		t.Errorf("projected_full_date = %v, want 2026-12-10", near["projected_full_date"])
	}
	far := map[string]any{"days_until_full": 2e5, "as_of_day": asOf}
	if got := projectedFull(far); got != "" {
		t.Errorf("projectedFull(200000 days) = %q, want no date", got)
	}
	if _, set := far["projected_full_date"]; set {
		t.Errorf("projected_full_date set for a 200000-day runway: %v", far)
	}
	if got := projectedFull(map[string]any{"days_until_full": 30.0}); got != "" {
		t.Errorf("projectedFull without as_of_day = %q, want no date", got)
	}
}
