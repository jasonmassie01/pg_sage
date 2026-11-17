package workpack

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// evid-02a: a pack's window is [from, to) on UTC hours, 14 to 100 days;
// days older than the hourly retention read the daily tier and say so.
// No DB or concurrency test here: the window and the day plan are pure.

var monday = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func TestWindow_RejectsEachBoundWithErrWindow(t *testing.T) {
	cases := map[string]struct {
		w    Window
		want string
	}{
		"13 days 23 hours": {Window{monday, monday.Add(14*24*time.Hour - time.Hour)}, "14 days"},
		"100 days 1 hour":  {Window{monday, monday.Add(100*24*time.Hour + time.Hour)}, "100 days"},
		"from equals to":   {Window{monday, monday}, "before"},
		"from after to":    {Window{monday.Add(time.Hour), monday}, "before"},
		"unaligned from": {Window{monday.Add(time.Minute),
			monday.Add(20 * 24 * time.Hour)}, "hour"},
		"unaligned to": {Window{monday, monday.Add(20*24*time.Hour + time.Second)}, "hour"},
		"zero":         {Window{}, "from"},
	}
	for name, c := range cases {
		err := c.w.Validate()
		if !errors.Is(err, ErrWindow) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want ErrWindow naming %q", name, err, c.want)
		}
	}
}

func TestWindow_ExactBoundsAreAccepted(t *testing.T) {
	for _, d := range []time.Duration{14 * 24 * time.Hour, 100 * 24 * time.Hour,
		DefaultWindow} {
		if err := (Window{monday, monday.Add(d)}).Validate(); err != nil {
			t.Errorf("window of %v: %v, want accepted", d, err)
		}
	}
	if DefaultWindow != 28*24*time.Hour {
		t.Fatalf("default window %v, want 28 days (baseline.MinWeeks plus one)",
			DefaultWindow)
	}
}

func TestWindow_NonUTCInstantsAreTheSameWindow(t *testing.T) {
	zone := time.FixedZone("plus5", 5*3600)
	w := Window{monday.In(zone), monday.Add(20 * 24 * time.Hour).In(zone)}
	if err := w.Validate(); err != nil {
		t.Fatalf("an hour-aligned window in another zone: %v", err)
	}
	if got := w.UTC(); got.From.Location() != time.UTC || !got.From.Equal(monday) {
		t.Fatalf("UTC() = %+v, want the same instants in UTC", got)
	}
}

func TestDefaultWindowEnding_IsTheLast28WholeHours(t *testing.T) {
	now := monday.Add(30*24*time.Hour + 17*time.Minute)
	w := DefaultWindowEnding(now)
	if !w.To.Equal(monday.Add(30*24*time.Hour)) || w.To.Sub(w.From) != DefaultWindow {
		t.Fatalf("default window %+v, want 28 days ending at the current hour", w)
	}
}

// The plan's example: built at 23:00 the day before the window ends, the
// purge has reached at most 2026-01-25 00:00, so days 1-25 are daily.
func TestPlanDays_SixtyDaysWithThirtyFiveHourlyListsDays1To25Daily(t *testing.T) {
	to := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	w := Window{to.Add(-60 * 24 * time.Hour), to}
	p := planDays(w, 35, time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC))
	if len(p.days) != 61 {
		t.Fatalf("%d days listed, want 61 (a partial first and last day)", len(p.days))
	}
	for i, d := range p.days {
		want := granularityHourly
		if i < 25 {
			want = granularityDaily
		}
		if d.Granularity != want {
			t.Fatalf("day %d (%s) is %s, want %s", i+1, d.Day, d.Granularity, want)
		}
	}
	if p.days[0].Hours != 12 || p.days[0].Counted {
		t.Fatalf("first day %+v: 12 window hours of a daily day cannot be counted",
			p.days[0])
	}
	if !p.days[1].Counted || p.days[1].Hours != 24 {
		t.Fatalf("second day %+v, want a whole counted daily day", p.days[1])
	}
	if len(p.dailyDays) != 24 || p.dailyDays[0] != "2026-01-01" {
		t.Fatalf("daily days %v, want the 24 whole days from 2026-01-01", p.dailyDays)
	}
	if !p.hourlyFrom.Equal(time.Date(2026, 1, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("hourly part starts %v, want 2026-01-25", p.hourlyFrom)
	}
}

func TestPlanDays_ZeroRetentionAndAFreshWindowAreAllHourly(t *testing.T) {
	w := Window{monday, monday.Add(28 * 24 * time.Hour)}
	for _, days := range []int{0, 35} {
		p := planDays(w, days, w.To)
		if len(p.dailyDays) != 0 || !p.hourlyFrom.Equal(w.From) {
			t.Fatalf("hourly days %d: plan %+v, want all hourly", days, p)
		}
		for _, d := range p.days {
			if d.Granularity != granularityHourly || !d.Counted {
				t.Fatalf("hourly days %d: day %+v, want hourly and counted", days, d)
			}
		}
	}
}

func TestPlanDays_AWindowWhollyPastTheHourlyRetentionIsDaily(t *testing.T) {
	w := Window{monday, monday.Add(20 * 24 * time.Hour)}
	p := planDays(w, 35, monday.Add(200*24*time.Hour))
	if !p.hourlyFrom.Equal(w.To) || len(p.dailyDays) != 20 {
		t.Fatalf("plan %+v, want no hourly part and 20 daily days", p)
	}
}

// The hourly purge is rolling (hour < now() - hourly days), so at a
// mid-day build the first UTC day inside the hourly retention has already
// lost its morning hours: that day is read from the daily tier, and the
// hourly part starts at the first whole day the purge has not reached.
// A build at midnight keeps the plan's days 1-25 daily.
func TestPlanDays_ThePartlyPurgedHourlyDayIsDaily(t *testing.T) {
	mid := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	w := Window{mid.Add(-60 * 24 * time.Hour), mid}
	p := planDays(w, 35, mid)
	// now - 35 days is 2026-01-25 12:00: hours 00:00-11:00 of 01-25 are gone.
	if !p.hourlyFrom.Equal(time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("mid-day build: hourly part starts %v, want 2026-01-26", p.hourlyFrom)
	}
	if d := p.days[25]; d.Day != "2026-01-25" || d.Granularity != granularityDaily ||
		!d.Counted || d.Hours != 24 {
		t.Fatalf("day 26 %+v, want 2026-01-25 daily and counted", d)
	}
	if len(p.dailyDays) != 25 || p.dailyDays[24] != "2026-01-25" {
		t.Fatalf("daily days %v, want the 25 whole days 2026-01-01..2026-01-25", p.dailyDays)
	}
	// The purge's "35 days" is calendar days in its session time zone, so
	// across a DST change its boundary sits up to an hour later: at 23:00 it
	// is at most 01-25 00:00 (01-25 stays hourly), at 23:30 it may be
	// 01-25 00:30 (01-25 is daily too).
	midnight := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	w = Window{midnight.Add(-60 * 24 * time.Hour), midnight}
	for _, c := range []struct {
		now  time.Time
		from time.Time
	}{
		{midnight.Add(-time.Hour), time.Date(2026, 1, 25, 0, 0, 0, 0, time.UTC)},
		{midnight.Add(-30 * time.Minute), time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)},
		{midnight, time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)},
	} {
		p = planDays(w, 35, c.now)
		daily := int(c.from.Sub(w.From) / (24 * time.Hour))
		if !p.hourlyFrom.Equal(c.from) || len(p.dailyDays) != daily ||
			p.days[daily-1].Granularity != granularityDaily ||
			p.days[daily].Granularity != granularityHourly {
			t.Fatalf("build at %v: hourly from %v with %d daily days, want %v and %d",
				c.now, p.hourlyFrom, len(p.dailyDays), c.from, daily)
		}
	}
}
