package policy

import (
	"strings"
	"testing"
	"time"
)

// D2: policy.ParseWindow is the single maintenance-window engine. It
// accepts the config grammar (presets, day names, lists and ranges), keeps
// its previous-day midnight wrap, gives cron windows a one-hour width (or
// an explicit "@<duration>"), and takes an optional trailing IANA zone.

// 2026-09-26 is a Saturday, 2026-09-27 a Sunday, 2026-09-28 a Monday.
func utc(day, hour, minute int) time.Time {
	return time.Date(2026, 9, day, hour, minute, 0, 0, time.UTC)
}

type windowCase struct {
	expr string
	at   time.Time
	want bool
}

func assertWindowCases(t *testing.T, cases []windowCase) {
	t.Helper()
	for _, c := range cases {
		window, err := ParseWindow(c.expr)
		if err != nil {
			t.Errorf("ParseWindow(%q): %v", c.expr, err)
			continue
		}
		if got := window.Contains(c.at); got != c.want {
			t.Errorf("ParseWindow(%q).Contains(%s) = %v, want %v",
				c.expr, c.at.Format("Mon 2006-01-02 15:04:05 MST"), got, c.want)
		}
	}
}

// T1: the documented example "0 2 * * *" is 02:00-03:00, not one minute.
func TestCronWindowIsOneHourWide(t *testing.T) {
	assertWindowCases(t, []windowCase{
		{"0 2 * * *", utc(28, 2, 0), true},
		{"0 2 * * *", utc(28, 2, 30), true},
		{"0 2 * * *", utc(28, 2, 59).Add(59 * time.Second), true},
		{"0 2 * * *", utc(28, 3, 0), false},
		{"0 2 * * *", utc(28, 1, 59), false},
		// The day fields are matched at the opening minute: a Friday 23:30
		// window runs into Saturday.
		{"30 23 * * 5", utc(26, 0, 15), true},
		{"30 23 * * 5", utc(26, 0, 30), false},
		{"30 23 * * 6", utc(26, 0, 15), false},
	})
}

func TestCronWindowExplicitDuration(t *testing.T) {
	assertWindowCases(t, []windowCase{
		{"0 2 * * * @1m", utc(28, 2, 0).Add(30 * time.Second), true},
		{"0 2 * * * @1m", utc(28, 2, 1), false},
		{"0 2 * * * @90m", utc(28, 3, 29), true},
		{"0 2 * * * @90m", utc(28, 3, 30), false},
		{"0 2 * * * @2h", utc(28, 3, 59), true},
		{"0 2 * * * @24h", utc(29, 1, 59), true},
		{"0 2 * * * @1h", utc(28, 3, 0), false},
		{"0 2 * * * @1H", utc(28, 2, 30), true},
	})
}

// T3: the config grammar's forms parse.
func TestParseWindowAcceptsConfigGrammar(t *testing.T) {
	for _, expr := range []string{
		"weeknights", "nights", "nightly", "overnight", "weekdays", "weekday",
		"weekends", "weekend", "business-hours", "off-hours", "off-peak",
		"daily 01:00-05:00", "everyday 01:00-02:00", "Mon-Fri 01:00-05:00",
		"sat,sun 02:00-06:00", "Mon,Wed,Fri 22:00-04:00", "fri-mon 00:00-06:00",
		"WEEKDAYS 01:00-05:00", "Weeknights", "daily", "sat,sun",
		"always", "ANYTIME", "24x7", "24/7",
	} {
		if _, err := ParseWindow(expr); err != nil {
			t.Errorf("ParseWindow(%q): %v", expr, err)
		}
	}
}

// T4: a range that wraps midnight belongs to the day it starts on.
func TestWeeknightsWrapToPreviousDay(t *testing.T) {
	assertWindowCases(t, []windowCase{
		{"weeknights", utc(26, 2, 0), true},   // Friday night
		{"weeknights", utc(28, 2, 0), false},  // Sunday night
		{"weeknights", utc(28, 23, 0), true},  // Monday night
		{"weeknights", utc(26, 23, 0), false}, // Saturday night
		{"weeknights", utc(28, 12, 0), false},
	})
}

// The table from the D2 memo §3, with the unified meaning.
func TestParseWindowMemoTable(t *testing.T) {
	assertWindowCases(t, []windowCase{
		{"0 2 * * *", utc(28, 2, 30), true},
		{"0 2 * * 0", utc(27, 2, 30), true},
		{"30 * * * *", utc(28, 5, 45), true},
		{"30 * * * *", utc(28, 5, 10), true},
		{"0 2 * * 1-5", utc(28, 2, 0), true},
		{"*/15 2 * * *", utc(28, 2, 15), true},
		{"weekdays 22:00-06:00", utc(26, 2, 0), true},
		{"weekdays 22:00-06:00", utc(28, 2, 0), false},
		{"daily 01:00-05:00", utc(28, 2, 0), true},
		{"Mon-Fri 01:00-05:00", utc(28, 2, 0), true},
		{"Mon-Fri 01:00-05:00", utc(26, 2, 0), false},
		{"sat,sun 02:00-06:00", utc(26, 3, 0), true},
		{"sat,sun 02:00-06:00", utc(28, 3, 0), false},
		{"22:00-02:00", utc(26, 23, 0), true},
		{"always", utc(28, 12, 0), true},
		{"weekends", utc(26, 12, 0), true},
		{"weekends", utc(28, 12, 0), false},
		{"weekdays 01:00-05:00", utc(28, 2, 0), true},
	})
}

func TestWindowBoundaries(t *testing.T) {
	assertWindowCases(t, []windowCase{
		{"00:00-23:59", utc(28, 0, 0), true},
		{"00:00-23:59", utc(28, 23, 58), true},
		{"00:00-23:59", utc(28, 23, 59), false},
		{"23:59-00:00", utc(28, 23, 59), true},
		{"23:59-00:00", utc(28, 0, 0), false},
		{"23:59-00:00", utc(28, 23, 58), false},
		{"daily 23:00-01:00", utc(26, 0, 30), true},
		{"weekdays 23:00-01:00", utc(26, 0, 30), true},  // Friday night
		{"weekdays 23:00-01:00", utc(28, 0, 30), false}, // Sunday night
		{"weekdays 23:00-01:00", utc(29, 0, 59), true},
		{"weekdays 23:00-01:00", utc(29, 1, 0), false},
		{"weekdays 01:00-05:00", utc(28, 1, 0), true},
		{"weekdays 01:00-05:00", utc(28, 5, 0), false},
		{"sat", utc(26, 0, 0), true},
		{"sat", utc(26, 23, 59), true},
		{"sat", utc(27, 0, 0), false},
	})
}

// T5: a trailing IANA zone evaluates the window on that zone's wall clock.
func TestWindowTimezone(t *testing.T) {
	assertWindowCases(t, []windowCase{
		// Monday 02:00 EDT.
		{"weekdays 01:00-05:00 America/New_York", utc(28, 6, 0), true},
		// Sunday 22:00 EDT.
		{"weekdays 01:00-05:00 America/New_York", utc(28, 2, 0), false},
		{"0 2 * * * America/New_York", utc(28, 6, 30), true},
		{"0 2 * * * @1m America/New_York", utc(28, 6, 30), false},
		{"weeknights Asia/Tokyo", utc(28, 14, 0), true}, // Mon 23:00 JST
		{"always UTC", utc(28, 14, 0), true},
	})
}

// Without a zone the window reads the instant's own location, which is the
// process-local clock the gate passes in: today's behaviour.
func TestWindowWithoutZoneUsesInstantLocation(t *testing.T) {
	chicagoish := time.FixedZone("UTC-5", -5*60*60)
	local := time.Date(2026, 9, 28, 2, 0, 0, 0, chicagoish)
	window, err := ParseWindow("weekdays 01:00-05:00")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	if !window.Contains(local) || window.Contains(local.UTC()) {
		t.Fatalf("zoneless window must use the instant's location: local=%v utc=%v",
			window.Contains(local), window.Contains(local.UTC()))
	}
}

// America/Chicago: 2026-03-08 02:00 CST jumps to 03:00 CDT; 2026-11-01
// 02:00 CDT falls back to 01:00 CST.
func TestWindowTimezoneDST(t *testing.T) {
	at := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	const chicago = "daily 01:00-04:00 America/Chicago"
	assertWindowCases(t, []windowCase{
		{chicago, at(time.March, 8, 6, 59), false}, // 00:59 CST
		{chicago, at(time.March, 8, 7, 30), true},  // 01:30 CST
		{chicago, at(time.March, 8, 8, 30), true},  // 03:30 CDT
		{chicago, at(time.March, 8, 9, 0), false},  // 04:00 CDT
		{chicago, at(time.November, 1, 6, 30), true},
		{chicago, at(time.November, 1, 7, 30), true}, // 01:30 CST, second pass
		{chicago, at(time.November, 1, 9, 0), true},  // 03:00 CST
		{chicago, at(time.November, 1, 10, 0), false},
		// 02:30 does not exist on the spring-forward day: no window opens.
		{"30 2 * * * America/Chicago", at(time.March, 8, 8, 45), false},
		{"30 2 * * * America/Chicago", at(time.March, 9, 8, 0), true},
		// 01:30 happens twice on the fall-back day; each opens a window.
		{"30 1 * * * America/Chicago", at(time.November, 1, 7, 45), true},
	})
}

func TestParseWindowRejectsMalformed(t *testing.T) {
	for _, expr := range []string{
		"", "   ", "weeknigths", "never", "weekdays 01:00", "Mon-Funday 01:00-02:00",
		"daily 1-2", "0 2 * *", "0 2 * * * *", "00:00-00:00", "24:00-01:00",
		"weekdays 01:00-05:00 extra", ",,, 01:00-02:00", "mon- 01:00-02:00",
		"weeknights 01:00-02:00", "0 2 * * * @0m", "0 2 * * * @25h",
		"0 2 * * * @30s", "0 2 * * * @1h30s", "0 2 * * * @abc", "0 2 * * * @-1h",
		"0 2 * * * @", "0 2 * * * @1h @2h", "@1h", "weekdays 01:00-05:00 @1h",
		"weekdays 01:00-05:00 Mars/Olympus", "weekdays 01:00-05:00 Local",
		"America/Chicago", "0 2 * * * America/Chicago @1h",
	} {
		t.Run(expr, func(t *testing.T) {
			_, err := ParseWindow(expr)
			if err == nil {
				t.Fatalf("ParseWindow(%q) = nil error, want rejection", expr)
			}
			if !strings.Contains(err.Error(), "window") {
				t.Fatalf("error %q does not say it is about a window", err)
			}
		})
	}
}

func TestParseWindowErrorsAreActionable(t *testing.T) {
	_, err := ParseWindow("weeknigths")
	if err == nil || !strings.Contains(err.Error(), "weeknigths") ||
		!strings.Contains(err.Error(), "weeknights") {
		t.Fatalf("typo error %v must quote the input and list the presets", err)
	}
	_, err = ParseWindow("weekdays 01:00-05:00 Mars/Olympus")
	if err == nil || !strings.Contains(err.Error(), "time zone") {
		t.Fatalf("zone error %v must name the time zone", err)
	}
}

func TestValidateDocumentWindows(t *testing.T) {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00 America/Chicago", "0 2 * * 0 @2h"}
	if err := ValidateDocument(doc); err != nil {
		t.Fatalf("ValidateDocument(zoned windows): %v", err)
	}
	for _, windows := range [][]string{nil, {}, {""}, {"weeknigths"}} {
		doc.MaintenanceWindows = windows
		if err := ValidateDocument(doc); err == nil {
			t.Fatalf("ValidateDocument(windows=%q) = nil, want error", windows)
		}
	}
}
