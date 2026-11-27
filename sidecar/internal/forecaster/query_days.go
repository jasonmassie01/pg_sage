package forecaster

import (
	"math"
	"time"
)

// wowDays is the two weeks query volume compares: the 7 closed days
// ending yesterday against the 7 before them (forecast-08).
const wowDays = 14

// closedQueryDays returns the calendar of closed days of a query read,
// oldest first, a missing or uncovered day as an Uncovered entry without
// calls. A day's first sample counts the calls since the previous sample,
// which lies on an earlier day; that delta is divided by its hours and
// each day keeps the share of the hours it holds, so a collector gap does
// not pile the gap's calls onto the day after it.
func closedQueryDays(aggs []DayQueryAgg, today time.Time, interval time.Duration) []DayQueryAgg {
	rows := make([]dayRow, len(aggs))
	for i, a := range aggs {
		rows[i] = dayRow{Day: a.Day, Samples: a.Samples, FirstAt: a.FirstAt, LastAt: a.LastAt}
	}
	days := closedDays(rows, today, interval)
	out := make([]DayQueryAgg, 0, len(days))
	for _, d := range days {
		if d.Row < 0 {
			out = append(out, DayQueryAgg{Day: d.Day, Uncovered: true})
			continue
		}
		a := aggs[d.Row]
		out = splitCarry(out, a)
		if !d.Covered {
			out[len(out)-1] = DayQueryAgg{Day: a.Day, Uncovered: true}
		}
	}
	// Today's first sample counted yesterday's last calls: yesterday gets
	// its share, today stays out.
	for _, a := range aggs {
		if dayOffset(a.Day, today) == 0 && len(out) > 0 {
			out = splitCarry(out, a)[:len(out)]
		}
	}
	return out
}

// splitCarry appends day a with its carried delta shared by hours: a
// keeps the hours after its start, the day the delta started on (when it
// is in out) the hours before. A day's end is the next day's start, or
// its own start plus 24 hours when the next day is missing.
func splitCarry(out []DayQueryAgg, a DayQueryAgg) []DayQueryAgg {
	span := a.CarryTo.Sub(a.CarryFrom)
	if a.CarryCalls == 0 || span <= 0 {
		return append(out, a)
	}
	share := func(from, to time.Time) float64 {
		h := math.Min(math.Max(to.Sub(from).Hours(), 0), span.Hours())
		return a.CarryCalls * h / span.Hours()
	}
	carry := a.CarryCalls
	a.TotalCalls += share(a.Day, a.CarryTo) - carry
	for i := len(out) - 1; i >= 0; i-- {
		if !out[i].Day.Equal(a.CarryFromDay) {
			continue
		}
		end := out[i].Day.Add(24 * time.Hour)
		if i == len(out)-1 {
			end = a.Day
		}
		if !out[i].Uncovered {
			out[i].TotalCalls += share(a.CarryFrom, end)
		}
		break
	}
	return append(out, a)
}

// wow is the week-over-week comparison of two complete weeks.
type wow struct {
	pct                float64
	thisWeek, lastWeek float64
	from, to           time.Time
}

// weekOverWeek compares the newest 7 days with the 7 before. It needs the
// newest wowDays entries to be consecutive covered calendar days (the
// closed calendar ends yesterday) and calls in the prior week; otherwise
// the category is not evaluated.
func weekOverWeek(days []DayQueryAgg) (wow, bool) {
	if len(days) < wowDays {
		return wow{}, false
	}
	window := days[len(days)-wowDays:]
	var w wow
	for i, d := range window {
		if d.Uncovered || i > 0 && dayOffset(window[i-1].Day, d.Day) != 1 {
			return wow{}, false
		}
		if i < wowDays/2 {
			w.lastWeek += d.TotalCalls
		} else {
			w.thisWeek += d.TotalCalls
		}
	}
	if !(w.lastWeek > 0) {
		return wow{}, false
	}
	w.pct = (w.thisWeek - w.lastWeek) / w.lastWeek * 100
	w.from, w.to = window[0].Day, window[wowDays-1].Day
	return w, true
}
