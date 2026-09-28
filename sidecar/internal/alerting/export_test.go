package alerting

// parseHour extracts the hour from "HH:MM"; -1 on empty or invalid
// input. Production code uses parseMinuteOfDay (minute precision,
// G7-B32); this wrapper keeps the existing hour-level tests.
func parseHour(s string) int {
	m := parseMinuteOfDay(s)
	if m < 0 {
		return -1
	}
	return m / 60
}
