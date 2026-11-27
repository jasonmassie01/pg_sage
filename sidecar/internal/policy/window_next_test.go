package policy

import (
	"testing"
	"time"
)

// policy-04: EffectiveWindow, NextOpen, ClosesAt and NextStart. The scan is
// bounded (8 days of minutes) and reports "none" past it.
//
// No concurrent access tests: an EffectiveWindow is an immutable value and
// every method is a pure function of it and the instant passed in.

func mustEffective(t *testing.T, trustConfigured bool, trust string,
	windows ...string) EffectiveWindow {
	t.Helper()
	window, err := NewEffectiveWindow(trustConfigured, trust, windows)
	if err != nil {
		t.Fatalf("NewEffectiveWindow(%v, %q, %q): %v", trustConfigured, trust, windows, err)
	}
	return window
}

func chicago(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	return location
}

func TestEffectiveWindowNextOpenAndClosesAt(t *testing.T) {
	utc := time.UTC
	at := func(day, hour, minute int) time.Time {
		// November 2026: the 13th is a Friday, the 14th a Saturday.
		return time.Date(2026, 11, day, hour, minute, 0, 0, utc)
	}
	cases := []struct {
		name      string
		window    EffectiveWindow
		from      time.Time
		wantOpen  time.Time
		wantClose time.Time
	}{
		{"wrap midnight opens tonight", mustEffective(t, false, "", "daily 22:00-06:00 UTC"),
			at(11, 14, 0), at(11, 22, 0), at(12, 6, 0)},
		{"wrap midnight already open", mustEffective(t, false, "", "daily 22:00-06:00 UTC"),
			at(12, 3, 30), at(12, 3, 30), at(12, 6, 0)},
		{"friday night wraps into saturday",
			mustEffective(t, false, "", "weekdays 22:00-06:00 UTC"),
			at(13, 12, 0), at(13, 22, 0), at(14, 6, 0)},
		{"saturday morning waits for monday night",
			mustEffective(t, false, "", "weekdays 22:00-06:00 UTC"),
			at(14, 7, 0), at(16, 22, 0), at(17, 6, 0)},
		{"cron with a 30 minute width", mustEffective(t, false, "", "0 2 * * * @30m UTC"),
			at(11, 2, 45), at(12, 2, 0), at(12, 2, 30)},
		{"intersection of trust and policy",
			mustEffective(t, true, "daily 03:00-04:00 UTC", "daily 01:00-05:00 UTC"),
			at(11, 1, 0), at(11, 3, 0), at(11, 4, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			open, ok := tc.window.NextOpen(tc.from)
			if !ok || !open.Equal(tc.wantOpen) {
				t.Fatalf("NextOpen(%s) = %s, %v; want %s", tc.from, open, ok, tc.wantOpen)
			}
			closes, ok := tc.window.ClosesAt(open)
			if !ok || !closes.Equal(tc.wantClose) {
				t.Fatalf("ClosesAt(%s) = %s, %v; want %s", open, closes, ok, tc.wantClose)
			}
		})
	}
}

func TestEffectiveWindowAlwaysNeverCloses(t *testing.T) {
	window := mustEffective(t, false, "", "always")
	from := time.Date(2026, 11, 11, 14, 7, 31, 0, time.UTC)
	open, ok := window.NextOpen(from)
	if !ok || !open.Equal(from) {
		t.Fatalf("NextOpen = %s, %v; want %s (open now)", open, ok, from)
	}
	if closes, ok := window.ClosesAt(from); ok {
		t.Fatalf("ClosesAt(always) = %s, true; want none within 8 days", closes)
	}
	start, closes, ok := window.NextStart(from, 12*time.Hour)
	if !ok || !start.Equal(from) || !closes.IsZero() {
		t.Fatalf("NextStart(always) = %s, %s, %v; want %s, zero, true",
			start, closes, ok, from)
	}
}

func TestEffectiveWindowIntersectionThatNeverOpens(t *testing.T) {
	window := mustEffective(t, true, "weekends UTC", "weekdays UTC")
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if open, ok := window.NextOpen(from); ok {
		t.Fatalf("NextOpen = %s, true; want none within 8 days", open)
	}
	if _, _, ok := window.NextStart(from, 0); ok {
		t.Fatal("NextStart found a start in a window that never opens")
	}
	if window.Contains(from) {
		t.Fatal("Contains = true for a never-open intersection")
	}
}

func TestEffectiveWindowConfiguredButEmptyTrustNeverOpens(t *testing.T) {
	// trust.maintenance_window "never" (or a disabled alias) reaches the
	// gate as configured with no expression: fail closed.
	window := mustEffective(t, true, "", "always")
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if _, ok := window.NextOpen(from); ok || window.Contains(from) {
		t.Fatal("a configured, empty trust window must never open")
	}
}

func TestEffectiveWindowUnconfiguredTrustIsIgnored(t *testing.T) {
	window := mustEffective(t, false, "weekends UTC", "always")
	// Wednesday: the unconfigured trust window must not narrow anything.
	from := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	if !window.Contains(from) {
		t.Fatal("an unconfigured trust window narrowed the effective window")
	}
}

func TestNewEffectiveWindowRejectsInvalidExpressions(t *testing.T) {
	if _, err := NewEffectiveWindow(true, "daily 25:00-26:00", []string{"always"}); err == nil {
		t.Fatal("an invalid trust window was accepted")
	}
	if _, err := NewEffectiveWindow(false, "", []string{"sometimes"}); err == nil {
		t.Fatal("an invalid policy window was accepted")
	}
	if _, err := NewEffectiveWindow(false, "", nil); err == nil {
		t.Fatal("no policy windows was accepted; the policy requires at least one")
	}
}

func TestEffectiveWindowDSTTransitions2026(t *testing.T) {
	zone := chicago(t)
	window := mustEffective(t, false, "", "daily 01:00-03:00 America/Chicago")
	// Spring forward, 2026-03-08 02:00 CST -> 03:00 CDT: the window is one
	// hour long (07:00Z-08:00Z).
	springFrom := time.Date(2026, 3, 7, 12, 0, 0, 0, zone)
	open, ok := window.NextOpen(springFrom)
	wantOpen := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)
	if !ok || !open.Equal(wantOpen) {
		t.Fatalf("spring NextOpen = %s, %v; want %s", open, ok, wantOpen)
	}
	closes, ok := window.ClosesAt(open)
	if wantClose := time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC); !ok ||
		!closes.Equal(wantClose) {
		t.Fatalf("spring ClosesAt = %s, %v; want %s", closes, ok, wantClose)
	}
	// Fall back, 2026-11-01 02:00 CDT -> 01:00 CST: 01:00-03:00 local spans
	// three real hours (06:00Z-09:00Z).
	fallFrom := time.Date(2026, 10, 31, 12, 0, 0, 0, zone)
	open, ok = window.NextOpen(fallFrom)
	wantOpen = time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	if !ok || !open.Equal(wantOpen) {
		t.Fatalf("fall NextOpen = %s, %v; want %s", open, ok, wantOpen)
	}
	closes, ok = window.ClosesAt(open)
	if wantClose := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC); !ok ||
		!closes.Equal(wantClose) {
		t.Fatalf("fall ClosesAt = %s, %v; want %s", closes, ok, wantClose)
	}
}

func TestEffectiveWindowSkippedLocalTimeOpensNextDay(t *testing.T) {
	// 02:00-02:30 does not exist in Chicago on 2026-03-08.
	window := mustEffective(t, false, "", "daily 02:00-02:30 America/Chicago")
	from := time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC) // 00:00 CST
	open, ok := window.NextOpen(from)
	want := time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC) // 02:00 CDT on 03-09
	if !ok || !open.Equal(want) {
		t.Fatalf("NextOpen = %s, %v; want %s", open, ok, want)
	}
}

func TestEffectiveWindowClosesAtWhenClosed(t *testing.T) {
	window := mustEffective(t, false, "", "daily 01:00-05:00 UTC")
	if closes, ok := window.ClosesAt(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("ClosesAt on a closed window = %s, true; want false", closes)
	}
	// Seconds inside the last minute still close at the minute boundary.
	closes, ok := window.ClosesAt(time.Date(2026, 11, 11, 4, 59, 30, 0, time.UTC))
	if want := time.Date(2026, 11, 11, 5, 0, 0, 0, time.UTC); !ok || !closes.Equal(want) {
		t.Fatalf("ClosesAt(04:59:30) = %s, %v; want %s", closes, ok, want)
	}
}

func TestEffectiveWindowNextStartHonoursMargin(t *testing.T) {
	window := mustEffective(t, false, "", "daily 01:00-05:00 UTC")
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	cases := []struct {
		name  string
		from  time.Time
		want  time.Time
		close time.Time
	}{
		{"exactly the margin left starts now", day(11, 4, 30), day(11, 4, 30), day(11, 5, 0)},
		{"one minute more than the margin starts now", day(11, 4, 29), day(11, 4, 29),
			day(11, 5, 0)},
		{"one minute under the margin waits a day", day(11, 4, 31), day(12, 1, 0),
			day(12, 5, 0)},
		{"closed waits for the opening", day(11, 12, 0), day(12, 1, 0), day(12, 5, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, closes, ok := window.NextStart(tc.from, 30*time.Minute)
			if !ok || !start.Equal(tc.want) || !closes.Equal(tc.close) {
				t.Fatalf("NextStart(%s) = %s, %s, %v; want %s, %s", tc.from, start, closes,
					ok, tc.want, tc.close)
			}
		})
	}
}

func TestEffectiveWindowNextStartMarginLongerThanWindow(t *testing.T) {
	window := mustEffective(t, false, "", "daily 01:00-02:00 UTC")
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if start, _, ok := window.NextStart(from, 2*time.Hour); ok {
		t.Fatalf("NextStart = %s, true; a 1h window can never fit a 2h margin", start)
	}
}

func TestEffectiveWindowZeroValueNeverOpens(t *testing.T) {
	var window EffectiveWindow
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if window.Contains(from) {
		t.Fatal("zero EffectiveWindow is open")
	}
	if _, ok := window.NextOpen(from); ok {
		t.Fatal("zero EffectiveWindow opens")
	}
}

func TestEffectiveWindowLocation(t *testing.T) {
	window := mustEffective(t, true, "weekdays 01:00-05:00 America/Chicago", "always")
	if got := window.Location(); got == nil || got.String() != "America/Chicago" {
		t.Fatalf("Location() = %v, want America/Chicago", got)
	}
	if got := mustEffective(t, false, "", "always").Location(); got != nil {
		t.Fatalf("Location() = %v, want nil for a window with no zone", got)
	}
}

func TestFormatWindowStart(t *testing.T) {
	at := time.Date(2026, 11, 17, 7, 0, 0, 0, time.UTC)
	got := FormatWindowStart(at, chicago(t))
	if want := "Tue 2026-11-17 01:00 America/Chicago"; got != want {
		t.Fatalf("FormatWindowStart = %q, want %q", got, want)
	}
	// No zone: the gate evaluates the process clock, so the time is shown in it.
	want := at.In(time.Local).Format("Mon 2006-01-02 15:04 MST")
	if got := FormatWindowStart(at, nil); got != want {
		t.Fatalf("FormatWindowStart(nil zone) = %q, want %q", got, want)
	}
}
