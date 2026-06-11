package executor

import (
	"testing"
	"time"
)

func TestInMaintenanceWindow_FriendlyForms(t *testing.T) {
	// Known weekdays: 2024-01-01 Mon, 01-03 Wed, 01-06 Sat.
	mk := func(y, m, d, hh, mm int) time.Time {
		return time.Date(y, time.Month(m), d, hh, mm, 0, 0, time.UTC)
	}
	monNight := mk(2024, 1, 1, 2, 0)   // Mon 02:00
	monNoon := mk(2024, 1, 1, 12, 0)   // Mon 12:00
	wedNight := mk(2024, 1, 3, 2, 0)   // Wed 02:00
	wedAM := mk(2024, 1, 3, 10, 0)     // Wed 10:00
	wedEve := mk(2024, 1, 3, 20, 0)    // Wed 20:00
	satNight := mk(2024, 1, 6, 3, 0)   // Sat 03:00
	satNoon := mk(2024, 1, 6, 14, 0)   // Sat 14:00

	cases := []struct {
		expr string
		now  time.Time
		want bool
	}{
		{"", monNight, false},
		{"always", monNoon, true},
		{"never", monNight, false},
		{"off", monNight, false},
		{"nights", monNight, true},      // 22:00-06:00
		{"nights", monNoon, false},
		{"weeknights", wedNight, true},  // weekdays 22:00-06:00
		{"weeknights", satNight, false}, // Sat is not a weeknight
		{"weekends", satNoon, true},     // all day Sat
		{"weekends", monNoon, false},
		{"weekdays", wedAM, true},
		{"weekdays", satNoon, false},
		{"business-hours", wedAM, true}, // weekdays 09:00-17:00
		{"business-hours", wedEve, false},
		{"business-hours", satNoon, false},
		{"weekdays 01:00-05:00", wedNight, true},
		{"weekdays 01:00-05:00", wedAM, false},
		{"weekdays 01:00-05:00", satNight, false}, // not a weekday
		{"Sat-Sun 02:00-06:00", satNight, true},
		{"Sat-Sun 02:00-06:00", wedNight, false},
		{"Mon,Wed,Fri 22:00-04:00", wedNight, true}, // Wed 02:00 in 22-04 wrap
		{"Mon,Wed,Fri 22:00-04:00", satNight, false},
		{"02:00-06:00", satNight, true},  // plain daily range still works
		{"0 2 * * *", mk(2024, 1, 1, 2, 30), true}, // cron still works
	}
	for _, c := range cases {
		if got := inMaintenanceWindowAt(c.expr, c.now); got != c.want {
			t.Errorf("inMaintenanceWindowAt(%q, %s) = %v, want %v",
				c.expr, c.now.Format("Mon 15:04"), got, c.want)
		}
	}
}
