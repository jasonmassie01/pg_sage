package config

import (
	"regexp"
	"strconv"
	"strings"
)

// dayTimeWindow matches "<days> HH:MM-HH:MM", optionally with a time zone.
var dayTimeWindow = regexp.MustCompile(
	`^([A-Za-z][A-Za-z,\-]*)\s+(\d{1,2}):(\d{2})-(\d{1,2}):(\d{2})(\s+\S+)?$`)

// MaintenanceWindowMeaningChange explains how the unified window grammar
// (D2, v1.7) reads a trust.maintenance_window value differently from the
// config parser it replaced, or returns "" when the meaning is unchanged.
func MaintenanceWindowMeaningChange(value string) string {
	v := strings.TrimSpace(value)
	if strings.EqualFold(v, "weeknights") {
		return "weeknights now opens on the night that starts each weekday " +
			"(Mon-Fri 22:00 to 06:00 the next morning, so Friday night runs into " +
			"Saturday); it previously ran Sunday night to Friday morning"
	}
	if note := dayWindowMeaningChange(v); note != "" {
		return note
	}
	return cronWindowMeaningChange(v)
}

func dayWindowMeaningChange(v string) string {
	m := dayTimeWindow.FindStringSubmatch(v)
	if m == nil || strings.EqualFold(m[1], "daily") {
		return ""
	}
	start := minutesOf(m[2], m[3])
	end := minutesOf(m[4], m[5])
	if end >= start {
		return ""
	}
	return "a day window that crosses midnight now belongs to the night it starts " +
		"(the listed day's evening into the next morning); it previously matched " +
		"the early hours of the listed day"
}

func cronWindowMeaningChange(v string) string {
	fields := strings.Fields(v)
	if len(fields) != 5 || !cronLike(fields[0]) {
		return ""
	}
	for _, field := range fields {
		if strings.ContainsAny(field, "-,/") {
			return "cron ranges, lists or steps now open the window at each match " +
				"for one hour; the old config parser treated them as never"
		}
	}
	if fields[1] == "*" && fields[0] != "*" {
		return "a cron window with any hour now opens every hour for one hour " +
			"from that minute; it previously ran only to the end of each hour"
	}
	return ""
}

func cronLike(field string) bool {
	return field == "*" || strings.IndexFunc(field, func(r rune) bool {
		return !strings.ContainsRune("0123456789*-,/", r)
	}) < 0
}

func minutesOf(hour, minute string) int {
	h, _ := strconv.Atoi(hour)
	m, _ := strconv.Atoi(minute)
	return h*60 + m
}
