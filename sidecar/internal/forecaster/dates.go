package forecaster

import "time"

// dateLayout formats the calendar dates of finding titles and details.
const dateLayout = "2006-01-02"

// maxProjectedDays bounds a dated projection (about a century): further
// out a date says nothing, and a time.Duration would overflow.
const maxProjectedDays = 36500

// calendarDate is the session's calendar date of a day start (UTC
// midnight of that date). pgx returns a timestamptz in Go's Local zone,
// which can differ from the session's: a day start is the session's
// midnight, give or take a DST hour, so half a day later its date in any
// zone within 12 hours of the session's is the session's date (forecast-08).
func calendarDate(day time.Time) time.Time {
	d := day.Add(12 * time.Hour)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

// projectedDate is the calendar date `days` whole days after the day
// start; false when days is negative, NaN or beyond maxProjectedDays.
func projectedDate(day time.Time, days float64) (string, bool) {
	if !(days >= 0 && days <= maxProjectedDays) {
		return "", false
	}
	return calendarDate(day).AddDate(0, 0, int(days)).Format(dateLayout), true
}
