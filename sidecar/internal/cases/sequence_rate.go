package cases

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// Detail keys of a sequence_exhaustion finding that carry the runway
// forecast pg_sage measured for the same sequence. The cases API copies
// them from the open forecast_sequence_runway finding (playbooks-03b).
const (
	DetailRunwayRatePerS       = "runway_rate_per_s"
	DetailRunwaySecondsToLimit = "runway_seconds_to_limit"
	DetailRunwayReadAt         = "runway_read_at"
)

// sequenceRateNotMeasured is said when no runway forecast is attached:
// the script does not guess a rate.
const sequenceRateNotMeasured = "Rate: not measured yet (no runway forecast for this " +
	"sequence). Run step 1 again later and compare remaining_values."

// sequenceRateMaxDays is how far ahead a limit is still dated.
const sequenceRateMaxDays = 36500

// sequenceRate is a runway forecast read from a finding's detail. Every
// field is a number or a time, so its text is built by strconv and time
// formatting alone and no detail string reaches the script.
type sequenceRate struct {
	ratePerS   float64
	seconds    float64
	readAt     time.Time
	descending bool
}

// sequenceRateOf reads the attached forecast. ok is false unless the rate
// and the time to the limit are finite numbers, the time is positive and
// the read time is known. The runway samples a descending sequence
// negated, so its rate is positive; a falling ascending one is no
// forecast.
func sequenceRateOf(detail map[string]any) (sequenceRate, bool) {
	rate, okRate := finiteNumber(detail[DetailRunwayRatePerS])
	secs, okSecs := finiteNumber(detail[DetailRunwaySecondsToLimit])
	readAt, okRead := timeOf(detail[DetailRunwayReadAt])
	if !okRate || !okSecs || !okRead || secs <= 0 {
		return sequenceRate{}, false
	}
	inc, _ := finiteNumber(detail["increment"])
	r := sequenceRate{ratePerS: math.Abs(rate), seconds: secs, readAt: readAt.UTC(),
		descending: inc < 0}
	if rate < 0 && !r.descending {
		return sequenceRate{}, false
	}
	return r, true
}

// sentences are the rate and the time to the limit, without comment
// markers: the script puts each on its own comment line.
func (r sequenceRate) sentences() []string {
	verb := "rises"
	if r.descending {
		verb = "falls"
	}
	rate := strconv.FormatFloat(r.ratePerS, 'f', -1, 64)
	if r.ratePerS < 0.01 {
		rate = "under 0.01"
	}
	first := "Measured rate: " + verb + " " + rate + " values/s (runway forecast read " +
		r.readAt.Format(time.RFC3339) + ")."
	days := r.seconds / 86400
	if days > sequenceRateMaxDays {
		return []string{first, "At that rate the limit is more than 100 years away."}
	}
	when := "less than a day"
	if n := int(math.Round(days)); days >= 1 {
		when = "about " + strconv.Itoa(n) + " day"
		if n != 1 {
			when += "s"
		}
	}
	date := r.readAt.Add(time.Duration(r.seconds * float64(time.Second)))
	return []string{first, "At that rate the limit is reached in " + when + " (" +
		date.Format("2006-01-02") + ")."}
}

// sequenceRateComment is Step 1's rate comment, one line per sentence.
func sequenceRateComment(detail map[string]any) string {
	r, ok := sequenceRateOf(detail)
	if !ok {
		return "-- Rate: not measured yet (no runway forecast for this sequence).\n" +
			"-- Run step 1 again later and compare remaining_values.\n"
	}
	out := ""
	for _, s := range r.sentences() {
		out += "-- " + s + "\n"
	}
	return out
}

// sequenceRateSentence is the same statement for the PR body.
func sequenceRateSentence(detail map[string]any) string {
	r, ok := sequenceRateOf(detail)
	if !ok {
		return sequenceRateNotMeasured
	}
	s := r.sentences()
	return s[0] + " " + s[1]
}

// finiteNumber accepts only numeric values (as decoded from JSON or set by
// Go code), never text.
func finiteNumber(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// timeOf accepts a time or an RFC 3339 text (the time after a JSON round
// trip); the result is re-formatted, never copied.
func timeOf(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, t)
		return parsed, err == nil && !parsed.IsZero()
	}
	return time.Time{}, false
}
