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
// twice. Runs of minutes that cannot match (the rest of a day or an hour
// the cron excludes) are skipped whole (previousCandidate), so a preview
// over days of a 24 h width is not quadratic in minutes.
func (cron cronWindow) openedWithin(at time.Time, width time.Duration) bool {
	earliest := at.Add(-width)
	for opened := at.Truncate(time.Minute); opened.After(earliest); {
		local := wallClock(opened)
		if cron.matchesWall(local) {
			return true
		}
		opened = cron.previousCandidate(opened, local)
	}
	return false
}

// wallClock is t's local date and time as a UTC instant: its fields read
// without a zone lookup, which for dates past a zone's table evaluates the
// zone's rule on every call.
func wallClock(t time.Time) time.Time {
	_, offset := t.Zone()
	return t.Add(time.Duration(offset) * time.Second).UTC()
}

// previousCandidate is the latest minute before t (whose wall clock is
// local) that could match: past the start of t's local day or hour when
// the cron excludes it, or the previous allowed minute of the hour. A jump
// is taken only when no offset change (DST) falls inside it, checked by the
// wall clock moving exactly as far as the absolute time did; otherwise it
// steps one minute.
func (cron cronWindow) previousCandidate(t, local time.Time) time.Time {
	back, exact := local.Minute(), false
	switch {
	case !cron.month.values[int(local.Month())] || !cron.dayMatches(local):
		back = local.Hour()*60 + local.Minute()
	case cron.hour.values[local.Hour()]:
		for m := local.Minute() - 1; m >= 0; m-- {
			if cron.minute.values[m] {
				back, exact = local.Minute()-m, true
				break
			}
		}
	}
	if back > 0 {
		jump := time.Duration(back) * time.Minute
		if start := t.Add(-jump); wallClock(start).Equal(local.Add(-jump)) {
			if exact {
				return start
			}
			t = start
		}
	}
	return t.Add(-time.Minute)
}

func (cron cronWindow) matches(at time.Time) bool {
	return cron.matchesWall(wallClock(at))
}

// matchesWall is matches for a wall clock from wallClock.
func (cron cronWindow) matchesWall(local time.Time) bool {
	return cron.minute.values[local.Minute()] && cron.hour.values[local.Hour()] &&
		cron.month.values[int(local.Month())] && cron.dayMatches(local)
}

// dayMatches applies the day-of-month and weekday fields as cron does:
// both must match when either is a wildcard, either may when both are set.
func (cron cronWindow) dayMatches(at time.Time) bool {
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
