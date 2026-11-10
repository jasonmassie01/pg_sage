package alerting

import "github.com/pg-sage/sidecar/internal/config"

// parseHour extracts the hour from "HH:MM"; -1 on empty or invalid
// input. Production code uses config.QuietHourMinute (minute precision,
// G7-B32); this wrapper keeps the existing hour-level tests.
func parseHour(s string) int {
	m := config.QuietHourMinute(s)
	if m < 0 {
		return -1
	}
	return m / 60
}
