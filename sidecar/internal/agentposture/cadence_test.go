package agentposture

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDailyBoundary(t *testing.T) {
	cases := []struct {
		now, dailyAt, want string
	}{
		{"2026-10-09 03:00", "03:00", "2026-10-09 03:00"},
		{"2026-10-09 02:59", "03:00", "2026-10-08 03:00"},
		{"2026-10-09 23:59", "03:00", "2026-10-09 03:00"},
		{"2026-10-09 00:00", "00:00", "2026-10-09 00:00"},
		{"2026-10-09 12:00", "23:59", "2026-10-08 23:59"},
		{"2026-10-01 01:00", "3:05", "2026-09-30 03:05"},
	}
	for _, c := range cases {
		got, err := dailyBoundary(at(c.now), c.dailyAt)
		if err != nil {
			t.Fatalf("dailyBoundary(%s, %s): %v", c.now, c.dailyAt, err)
		}
		if !got.Equal(at(c.want)) {
			t.Errorf("dailyBoundary(%s, %s) = %s, want %s", c.now, c.dailyAt, got, c.want)
		}
	}
	if _, err := dailyBoundary(at("2026-10-09 12:00"), "noon"); err == nil {
		t.Fatal("an invalid daily_at was accepted")
	}
}

func TestDueReason(t *testing.T) {
	now := at("2026-10-09 12:00")
	ran := at("2026-10-09 04:00") // after today's 03:00 run boundary
	cases := []struct {
		name    string
		st      cadence
		fp      int64
		now     time.Time
		dailyAt string
		want    string
	}{
		{"first run", cadence{}, 7, now, "03:00", dueFirst},
		{"catalog changed", cadence{ran: ran, fp: 7, haveFP: true}, 8, now, "03:00",
			dueCatalog},
		{"unchanged, ran after the boundary", cadence{ran: ran, fp: 7, haveFP: true}, 7, now,
			"03:00", ""},
		{"daily boundary passed", cadence{ran: at("2026-10-09 02:00"), fp: 7, haveFP: true},
			7, now, "03:00", dueDaily},
		{"exactly at the boundary", cadence{ran: at("2026-10-09 02:59"), fp: 7,
			haveFP: true}, 7, at("2026-10-09 03:00"), "03:00", dueDaily},
		{"ran exactly at the boundary", cadence{ran: at("2026-10-09 03:00"), fp: 7,
			haveFP: true}, 7, at("2026-10-09 03:00"), "03:00", ""},
		{"no fingerprint yet but ran", cadence{ran: ran}, 7, now, "03:00", dueCatalog},
		// An invalid daily_at never stops the fingerprint trigger, and is
		// treated as due daily from the last run (fail towards checking).
		{"invalid daily_at, a day later", cadence{ran: at("2026-10-08 11:00"), fp: 7,
			haveFP: true}, 7, now, "bad", dueDaily},
		{"invalid daily_at, same day", cadence{ran: ran, fp: 7, haveFP: true}, 7, now,
			"bad", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.st.dueReason(c.fp, c.now, c.dailyAt); got != c.want {
				t.Fatalf("dueReason = %q, want %q", got, c.want)
			}
		})
	}
}

// State transitions: each run records the fingerprint and time it ran on,
// so the same catalog is not checked twice before the next daily run.
func TestCadence_RecordTransitions(t *testing.T) {
	var st cadence
	now := at("2026-10-09 12:00")
	if st.dueReason(1, now, "03:00") != dueFirst {
		t.Fatal("not due on first sight")
	}
	st.record(1, now)
	if r := st.dueReason(1, now.Add(time.Minute), "03:00"); r != "" {
		t.Fatalf("due again right after a run: %q", r)
	}
	if r := st.dueReason(2, now.Add(2*time.Minute), "03:00"); r != dueCatalog {
		t.Fatalf("catalog change not due: %q", r)
	}
	st.record(2, now.Add(2*time.Minute))
	if r := st.dueReason(2, at("2026-10-10 03:00"), "03:00"); r != dueDaily {
		t.Fatalf("next day's run not due: %q", r)
	}
}
