package policy

import (
	"strings"
	"time"
)

// parseDaySpec parses a day list: a name (daily, weekdays, weekends), a
// comma list (sat,sun) or ranges (Mon-Fri, fri-mon wraps). Case-insensitive.
func parseDaySpec(spec string) (map[time.Weekday]bool, bool) {
	switch strings.ToLower(spec) {
	case "daily", "everyday", "every-day", "all", "all-days":
		return allDays(), true
	case "weekdays", "weekday":
		return weekdayDays(), true
	case "weekends", "weekend":
		return weekendDays(), true
	}
	days := make(map[time.Weekday]bool, 7)
	for _, part := range strings.Split(spec, ",") {
		if !addDayPart(days, part) {
			return nil, false
		}
	}
	return days, true
}

func addDayPart(days map[time.Weekday]bool, part string) bool {
	startText, endText, isRange := strings.Cut(part, "-")
	start, ok := dayNumber(startText)
	if !ok {
		return false
	}
	if !isRange {
		days[start] = true
		return true
	}
	end, ok := dayNumber(endText)
	if !ok {
		return false
	}
	for day := start; ; day = (day + 1) % 7 { // inclusive; fri-mon wraps
		days[day] = true
		if day == end {
			return true
		}
	}
}

func dayNumber(name string) (time.Weekday, bool) {
	switch strings.ToLower(name) {
	case "sun", "sunday":
		return time.Sunday, true
	case "mon", "monday":
		return time.Monday, true
	case "tue", "tues", "tuesday":
		return time.Tuesday, true
	case "wed", "weds", "wednesday":
		return time.Wednesday, true
	case "thu", "thur", "thurs", "thursday":
		return time.Thursday, true
	case "fri", "friday":
		return time.Friday, true
	case "sat", "saturday":
		return time.Saturday, true
	}
	return 0, false
}

func weekdayDays() map[time.Weekday]bool {
	return map[time.Weekday]bool{
		time.Monday: true, time.Tuesday: true, time.Wednesday: true,
		time.Thursday: true, time.Friday: true,
	}
}

func weekendDays() map[time.Weekday]bool {
	return map[time.Weekday]bool{time.Saturday: true, time.Sunday: true}
}

func allDays() map[time.Weekday]bool {
	days := make(map[time.Weekday]bool, 7)
	for day := time.Sunday; day <= time.Saturday; day++ {
		days[day] = true
	}
	return days
}
