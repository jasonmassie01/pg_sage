package policy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Window struct {
	contains func(time.Time) bool
}

func (w Window) Contains(at time.Time) bool {
	return w.contains != nil && w.contains(at)
}

func ParseWindow(expression string) (Window, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return Window{}, fmt.Errorf("window expression is empty")
	}
	switch strings.ToLower(expression) {
	case "always", "anytime", "24x7", "24/7":
		return Window{contains: func(time.Time) bool { return true }}, nil
	case "weekends":
		return dayWindow(weekendDays(), fullDayRange()), nil
	}
	if window, ok, err := parseFriendlyWindow(expression); ok || err != nil {
		return window, err
	}
	return parseCronWindow(expression)
}

type minuteRange struct {
	start int
	end   int
}

func (r minuteRange) contains(minute int) bool {
	if r.start <= r.end {
		return minute >= r.start && minute < r.end
	}
	return minute >= r.start || minute < r.end
}

func parseFriendlyWindow(expression string) (Window, bool, error) {
	parts := strings.Fields(expression)
	if len(parts) == 1 && strings.Contains(parts[0], "-") {
		rangeValue, err := parseClockRange(parts[0])
		if err != nil {
			return Window{}, true, err
		}
		return dayWindow(allDays(), rangeValue), true, nil
	}
	if len(parts) != 2 {
		return Window{}, false, nil
	}
	days, ok := namedDays(parts[0])
	if !ok {
		return Window{}, false, nil
	}
	rangeValue, err := parseClockRange(parts[1])
	if err != nil {
		return Window{}, true, err
	}
	return dayWindow(days, rangeValue), true, nil
}

func parseClockRange(value string) (minuteRange, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return minuteRange{}, fmt.Errorf("window range %q is invalid", value)
	}
	start, err := parseClock(parts[0])
	if err != nil {
		return minuteRange{}, err
	}
	end, err := parseClock(parts[1])
	if err != nil {
		return minuteRange{}, err
	}
	if start == end {
		return minuteRange{}, fmt.Errorf("window range %q has zero duration", value)
	}
	return minuteRange{start: start, end: end}, nil
}

func parseClock(value string) (int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("window clock %q is invalid", value)
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 ||
		minute < 0 || minute > 59 {
		return 0, fmt.Errorf("window clock %q is invalid", value)
	}
	return hour*60 + minute, nil
}

func dayWindow(days map[time.Weekday]bool, active minuteRange) Window {
	return Window{contains: func(at time.Time) bool {
		minute := at.Hour()*60 + at.Minute()
		day := at.Weekday()
		if active.start > active.end && minute < active.end {
			day = (day + 6) % 7
		}
		return days[day] && active.contains(minute)
	}}
}

func namedDays(value string) (map[time.Weekday]bool, bool) {
	switch strings.ToLower(value) {
	case "weekdays":
		return weekdayDays(), true
	case "weekends":
		return weekendDays(), true
	default:
		return nil, false
	}
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

func fullDayRange() minuteRange {
	return minuteRange{start: 0, end: 24 * 60}
}

type cronWindow struct {
	minute cronField
	hour   cronField
	day    cronField
	month  cronField
	week   cronField
}

func parseCronWindow(expression string) (Window, error) {
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return Window{}, fmt.Errorf("window expression %q is invalid", expression)
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	fields := make([]cronField, 5)
	for i, part := range parts {
		field, err := parseCronField(part, bounds[i][0], bounds[i][1])
		if err != nil {
			return Window{}, fmt.Errorf("window cron field %q: %w", part, err)
		}
		fields[i] = field
	}
	cron := cronWindow{fields[0], fields[1], fields[2], fields[3], fields[4]}
	return Window{contains: cron.contains}, nil
}

type cronField struct {
	values   map[int]bool
	wildcard bool
}

func parseCronField(expression string, minValue, maxValue int) (cronField, error) {
	field := cronField{values: make(map[int]bool), wildcard: expression == "*"}
	for _, item := range strings.Split(expression, ",") {
		if err := addCronItem(&field, item, minValue, maxValue); err != nil {
			return cronField{}, err
		}
	}
	if len(field.values) == 0 {
		return cronField{}, fmt.Errorf("has no values")
	}
	return field, nil
}

func addCronItem(field *cronField, item string, minValue, maxValue int) error {
	base, step, err := splitStep(item)
	if err != nil {
		return err
	}
	start, end, err := cronBounds(base, minValue, maxValue)
	if err != nil {
		return err
	}
	for value := start; value <= end; value += step {
		field.values[value] = true
	}
	return nil
}

func splitStep(item string) (string, int, error) {
	parts := strings.Split(item, "/")
	if len(parts) > 2 {
		return "", 0, fmt.Errorf("invalid step %q", item)
	}
	if len(parts) == 1 {
		return parts[0], 1, nil
	}
	step, err := strconv.Atoi(parts[1])
	if err != nil || step <= 0 {
		return "", 0, fmt.Errorf("invalid step %q", item)
	}
	return parts[0], step, nil
}

func cronBounds(base string, minValue, maxValue int) (int, int, error) {
	if base == "*" {
		return minValue, maxValue, nil
	}
	parts := strings.Split(base, "-")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("invalid range %q", base)
	}
	start, err := boundedInteger(parts[0], minValue, maxValue)
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 1 {
		return start, start, nil
	}
	end, err := boundedInteger(parts[1], minValue, maxValue)
	if err != nil || end < start {
		return 0, 0, fmt.Errorf("invalid range %q", base)
	}
	return start, end, nil
}

func boundedInteger(value string, minValue, maxValue int) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < minValue || number > maxValue {
		return 0, fmt.Errorf("value %q is outside %d-%d", value, minValue, maxValue)
	}
	return number, nil
}

func (cron cronWindow) contains(at time.Time) bool {
	if !cron.minute.values[at.Minute()] || !cron.hour.values[at.Hour()] ||
		!cron.month.values[int(at.Month())] {
		return false
	}
	dayMatch := cron.day.values[at.Day()]
	weekDay := int(at.Weekday())
	weekMatch := cron.week.values[weekDay] || (weekDay == 0 && cron.week.values[7])
	if cron.day.wildcard || cron.week.wildcard {
		return (cron.day.wildcard || dayMatch) && (cron.week.wildcard || weekMatch)
	}
	return dayMatch || weekMatch
}
