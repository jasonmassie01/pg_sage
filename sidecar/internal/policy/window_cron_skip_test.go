package policy

import (
	"strings"
	"testing"
	"time"
)

// everyMinuteOpen is the reference: every absolute minute from width
// before from to end is tested once, and the minute i minutes after from
// is open when the latest matching minute at or before it is less than
// width ago.
func everyMinuteOpen(cron cronWindow, from, end time.Time, width time.Duration) []bool {
	var open []bool
	var last time.Time
	for m := from.Add(-width).Truncate(time.Minute); !m.After(end); m = m.Add(time.Minute) {
		if cron.matches(m) {
			last = m
		}
		if !m.Before(from) {
			open = append(open, !last.IsZero() && m.Sub(last) < width)
		}
	}
	return open
}

func mustCron(t *testing.T, expression string) cronWindow {
	t.Helper()
	fields := strings.Fields(expression)
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	parsed := make([]cronField, 5)
	for i, part := range fields {
		field, err := parseCronField(part, bounds[i][0], bounds[i][1])
		if err != nil {
			t.Fatalf("cron %q field %q: %v", expression, part, err)
		}
		parsed[i] = field
	}
	return cronWindow{parsed[0], parsed[1], parsed[2], parsed[3], parsed[4]}
}

// Boundary and state transitions: across DST changes (Chicago's one-hour
// shifts, Lord Howe's half-hour ones, Kathmandu's :45 offset) every minute
// of two days agrees with the minute-by-minute scan, for crons whose
// skipped runs (excluded days and hours) end right at a transition.
func TestCronOpenedWithin_SkippingAgreesWithEveryMinute(t *testing.T) {
	crons := []string{"0 0 29 2 *", "30 2 * * *", "0 3 * * 0", "15 1,2,3 * * *",
		"*/7 * 8 3 *", "0 0 1 * 1", "59 23 * * 6", "* 2 * * *"}
	zones := []struct {
		name string
		from time.Time
	}{
		{"America/Chicago", time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)},
		{"America/Chicago", time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)},
		{"Australia/Lord_Howe", time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC)},
		{"Australia/Lord_Howe", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{"Asia/Kathmandu", time.Date(2028, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"UTC", time.Date(2028, 2, 28, 0, 0, 0, 0, time.UTC)},
	}
	widths := []time.Duration{time.Minute, 61 * time.Minute, 24 * time.Hour}
	for _, z := range zones {
		loc, err := time.LoadLocation(z.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, expr := range crons {
			cron := mustCron(t, expr)
			for _, width := range widths {
				agreeOverTwoDays(t, cron, expr, z.from.In(loc), width)
			}
		}
	}
}

func agreeOverTwoDays(t *testing.T, cron cronWindow, expr string, from time.Time,
	width time.Duration) {
	t.Helper()
	end := from.Add(48 * time.Hour)
	want := everyMinuteOpen(cron, from, end, width)
	opened := 0
	for i, at := 0, from; !at.After(end); i, at = i+1, at.Add(time.Minute) {
		if got := cron.openedWithin(at, width); got != want[i] {
			t.Fatalf("%q @%s at %s: openedWithin = %t, every-minute scan = %t",
				expr, width, at.Format(time.RFC3339), got, want[i])
		}
		if want[i] {
			opened++
		}
	}
	if expr == "* 2 * * *" && opened == 0 {
		t.Fatalf("%q @%s from %s never opened: the comparison checks nothing",
			expr, width, from.Format(time.RFC3339))
	}
}

// Nil/empty: a cron that cannot match in the width is closed, and a
// matching minute exactly width ago no longer covers at.
func TestCronOpenedWithin_EdgesOfTheWidth(t *testing.T) {
	cron := mustCron(t, "0 2 * * *")
	at := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	if !cron.openedWithin(at, time.Minute) {
		t.Fatal("the matching minute itself is not open")
	}
	if cron.openedWithin(at.Add(time.Hour), time.Hour) {
		t.Fatal("a match exactly one width ago still covers at")
	}
	if !cron.openedWithin(at.Add(time.Hour-time.Minute), time.Hour) {
		t.Fatal("the last minute of the width is not open")
	}
	if mustCron(t, "0 0 30 2 *").openedWithin(at, 24*time.Hour) {
		t.Fatal("February 30 opened a window")
	}
}

// The skip makes the worst case linear: a 24 h width over an 8-day preview
// tests a few minutes per day and hour, not every minute of every width.
func TestCronOpenedWithin_NeverMatchingWideCronIsCheap(t *testing.T) {
	cron := mustCron(t, "0 0 29 2 *")
	calls := 0
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for at := from; at.Before(from.Add(PreviewHorizon)); at = at.Add(time.Minute) {
		opened := at.Truncate(time.Minute)
		for opened.After(at.Add(-24 * time.Hour)) {
			calls++
			opened = cron.previousCandidate(opened, wallClock(opened))
		}
	}
	minutes := int(PreviewHorizon / time.Minute)
	if calls > 4*minutes {
		t.Fatalf("%d candidate minutes over %d preview minutes, want at most 4 each",
			calls, minutes)
	}
}
