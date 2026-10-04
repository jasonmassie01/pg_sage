package snapstore_test

import (
	"testing"
	"time"
)

func TestOneDayWindowStaysInsideOneUTCDay(t *testing.T) {
	day := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		start time.Time
		want  time.Time
	}{
		{"midday unchanged", day.Add(12*time.Hour + 30*time.Second),
			day.Add(12 * time.Hour)},
		{"ends a minute before midnight", day.Add(22*time.Hour + 59*time.Minute),
			day.Add(22*time.Hour + 59*time.Minute)},
		{"ends exactly at midnight moves back", day.Add(23 * time.Hour),
			day.Add(22*time.Hour + 59*time.Minute)},
		{"crosses midnight moves back", day.Add(23*time.Hour + 20*time.Minute),
			day.Add(22*time.Hour + 59*time.Minute)},
		{"just after midnight unchanged", day.Add(24*time.Hour + time.Minute),
			day.Add(24*time.Hour + time.Minute)},
	}
	for _, tc := range cases {
		got := oneDayWindow(tc.start, time.Hour)
		if !got.Equal(tc.want) {
			t.Errorf("%s: oneDayWindow(%s) = %s, want %s", tc.name,
				tc.start.Format(time.RFC3339), got.Format(time.RFC3339),
				tc.want.Format(time.RFC3339))
		}
		end := got.Add(time.Hour)
		if !end.Before(got.Truncate(24 * time.Hour).Add(24 * time.Hour)) {
			t.Errorf("%s: window %s..%s reaches the next UTC day", tc.name,
				got.Format(time.RFC3339), end.Format(time.RFC3339))
		}
	}
}
