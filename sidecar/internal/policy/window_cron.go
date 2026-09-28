package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCronWidth = time.Hour
	maxCronWidth     = 24 * time.Hour
)

type cronWindow struct {
	minute cronField
	hour   cronField
	day    cronField
	month  cronField
	week   cronField
}

// parseCronWindow parses "<minute> <hour> <day> <month> <weekday>" with an
// optional trailing "@<duration>". Each matching minute opens a window of
// that duration (default one hour): "0 2 * * *" is 02:00-03:00. Stored
// policies written before this width existed were migrated to "@1m".
func parseCronWindow(fields []string) (func(time.Time) bool, error) {
	width := defaultCronWidth
	if last := fields[len(fields)-1]; strings.HasPrefix(last, "@") {
		parsed, err := parseCronWidth(last)
		if err != nil {
			return nil, err
		}
		width, fields = parsed, fields[:len(fields)-1]
	}
	if len(fields) != 5 {
		return nil, errors.New("a cron window needs 5 fields (minute hour day month " +
			"weekday); a @duration applies only to a cron window")
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	parsed := make([]cronField, 5)
	for i, part := range fields {
		field, err := parseCronField(part, bounds[i][0], bounds[i][1])
		if err != nil {
			return nil, fmt.Errorf("cron field %q: %w", part, err)
		}
		parsed[i] = field
	}
	cron := cronWindow{parsed[0], parsed[1], parsed[2], parsed[3], parsed[4]}
	return func(at time.Time) bool { return cron.openedWithin(at, width) }, nil
}

func parseCronWidth(token string) (time.Duration, error) {
	width, err := time.ParseDuration(strings.ToLower(strings.TrimPrefix(token, "@")))
	if err != nil || width < time.Minute || width > maxCronWidth ||
		width%time.Minute != 0 {
		return 0, fmt.Errorf("duration %q must be whole minutes from @1m to @24h", token)
	}
	return width, nil
}

// openedWithin reports whether a matching minute opened a window of width
// that still covers at. Minutes are stepped in absolute time, so a local
// time skipped by a DST change never matches and a repeated one matches
// twice.
func (cron cronWindow) openedWithin(at time.Time, width time.Duration) bool {
	earliest := at.Add(-width)
	for opened := at.Truncate(time.Minute); opened.After(earliest); {
		if cron.matches(opened) {
			return true
		}
		opened = opened.Add(-time.Minute)
	}
	return false
}

func (cron cronWindow) matches(at time.Time) bool {
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
