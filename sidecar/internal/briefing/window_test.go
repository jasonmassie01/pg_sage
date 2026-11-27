package briefing

import (
	"strings"
	"testing"
	"time"
)

// llm-06: a briefing covers the time since the previous one, not a fixed
// 24 hours.

// No concurrent access tests here: window computation is pure and reads no
// shared state; live_test.go covers the worker's settings under concurrency.

func mustCron(t *testing.T, expr string) cronSchedule {
	t.Helper()
	s, err := parseCron(expr)
	if err != nil {
		t.Fatalf("parseCron(%q): %v", expr, err)
	}
	return s
}

// Boundary: with no prior briefing, a daily cron reads one day back to the
// previous fire, a weekly cron reads seven.
func TestWindowFirstRunUsesPreviousCronFire(t *testing.T) {
	// 2026-11-09 is a Monday.
	now := time.Date(2026, 11, 9, 6, 5, 0, 0, time.UTC)
	tests := []struct {
		expr string
		want time.Time
	}{
		{"0 6 * * *", time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)},
		{"0 6 * * 1", time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 11, 9, 5, 45, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		w := &Worker{schedule: mustCron(t, tt.expr), lastRunLoaded: true}
		win := w.windowFor(now)
		if !win.Start.Equal(tt.want) {
			t.Errorf("%s: window start = %s, want %s", tt.expr, win.Start, tt.want)
		}
		if !win.End.Equal(now) {
			t.Errorf("%s: window end = %s, want now %s", tt.expr, win.End, now)
		}
	}
}

// Boundary: a run exactly on the fire minute reads back to the fire before.
func TestWindowOnTheFireMinuteReadsToThePreviousFire(t *testing.T) {
	now := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	w := &Worker{schedule: mustCron(t, "0 6 * * *"), lastRunLoaded: true}
	want := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	if got := w.windowFor(now).Start; !got.Equal(want) {
		t.Fatalf("window start = %s, want %s", got, want)
	}
}

// Happy path: the previous briefing's time starts the window.
func TestWindowStartsAtThePreviousBriefing(t *testing.T) {
	last := time.Date(2026, 11, 6, 6, 0, 12, 345000, time.UTC)
	now := last.Add(72 * time.Hour)
	w := &Worker{schedule: mustCron(t, "0 6 * * *"), lastRun: last, lastRunLoaded: true}
	if got := w.windowFor(now).Start; !got.Equal(last) {
		t.Fatalf("window start = %s, want the previous briefing %s", got, last)
	}
}

// Invalid input and nil: an invalid or never-matching schedule falls back
// to 24 hours rather than an empty or unbounded window.
func TestWindowFallsBackTo24Hours(t *testing.T) {
	now := time.Date(2026, 11, 9, 6, 5, 0, 0, time.UTC)
	never := mustCron(t, "0 6 30 2 *") // 30 February never happens
	for name, w := range map[string]*Worker{
		"invalid":  {schedule: cronSchedule{}, lastRunLoaded: true},
		"never":    {schedule: never, lastRunLoaded: true},
		"zero":     {lastRunLoaded: true},
		"future":   {schedule: never, lastRun: now.Add(time.Hour), lastRunLoaded: true},
		"exactnow": {schedule: never, lastRun: now, lastRunLoaded: true},
	} {
		got := w.windowFor(now)
		if want := now.Add(-24 * time.Hour); !got.Start.Equal(want) {
			t.Errorf("%s: window start = %s, want 24h fallback %s", name, got.Start, want)
		}
	}
}

// The structured heading names the window it covers.
func TestWindowLabel(t *testing.T) {
	end := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	tests := []struct {
		start time.Time
		want  string
	}{
		{time.Time{}, "24h"},
		{end.Add(-24 * time.Hour), "24h"},
		{end.Add(-7 * 24 * time.Hour), "7d"},
		{end.Add(-3 * time.Hour), "3h"},
		{end.Add(-90 * time.Minute), "since 2026-11-09 04:30 UTC"},
	}
	for _, tt := range tests {
		got := Window{Start: tt.start, End: end}.label()
		if got != tt.want {
			t.Errorf("label(%s) = %q, want %q", end.Sub(tt.start), got, tt.want)
		}
	}
}

func TestStructuredRecentActionsHeadingNamesTheWindow(t *testing.T) {
	end := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	w := newTestWorker(nil)
	w.window = Window{Start: end.Add(-7 * 24 * time.Hour), End: end}
	out := w.buildStructured("[]", 0, `{}`,
		`[{"action_type":"create_index","outcome":"success"}]`)
	if !strings.Contains(out, "## Recent Actions (7d)") {
		t.Fatalf("heading does not name the 7-day window:\n%s", out)
	}
}

// A briefing that failed to store leaves no stale time behind: MarkRan
// then falls back to now instead of reusing an older briefing's time.
func TestMarkRanUsesTheStoredBriefingTime(t *testing.T) {
	stored := time.Date(2026, 11, 9, 6, 0, 1, 0, time.UTC)
	w := &Worker{lastGenerated: stored}
	w.MarkRan()
	if !w.lastRun.Equal(stored) {
		t.Fatalf("lastRun = %s, want the stored generated_at %s", w.lastRun, stored)
	}
	w.lastGenerated = time.Time{}
	before := time.Now()
	w.MarkRan()
	if w.lastRun.Before(before) {
		t.Fatalf("lastRun = %s, want now (no stored briefing)", w.lastRun)
	}
}
