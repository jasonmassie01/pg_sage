package cases

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// headroom is an unassessed reading that still shows what was measured.
func headroom(basis, unknown string, inputs map[string]any) urgencyRead {
	return urgencyRead{urgency: Urgency{Basis: basis, Inputs: inputs},
		whyNow: basis + "; " + unknown}
}

// xidHeadroom: an XID age with no consumption rate is headroom, not a
// deadline.
func xidHeadroom(d map[string]any) urgencyRead {
	age, okAge := positiveField(d, "xid_age")
	critical, okCritical := positiveField(d, "critical_threshold")
	if !okAge || !okCritical {
		return urgencyRead{whyNow: notAssessed}
	}
	basis := fmt.Sprintf("xid_age %s of the %s critical threshold",
		approxCount(age), approxCount(critical))
	return headroom(basis, "no consumption rate is measured, so the time to "+
		"wraparound is not projected", pick(d, "xid_age", "warning_threshold",
		"critical_threshold"))
}

const sequenceUnknown = "time to exhaustion and application impact have not been established."

// sequenceHeadroom: a consumed share with no consumption rate.
func sequenceHeadroom(d map[string]any) urgencyRead {
	pct, ok := positiveField(d, "usage_pct")
	if !ok {
		return urgencyRead{whyNow: "The observed sequence is near its configured limit; " +
			sequenceUnknown}
	}
	basis := fmt.Sprintf("usage_pct %s of the sequence range",
		strconv.FormatFloat(math.Round(pct*10)/10, 'f', -1, 64))
	return headroom(basis, sequenceUnknown, pick(d, "usage_pct", "current_value",
		"max_value", "increment"))
}

// overdueHeadroom: a table already past its freeze maximum has no
// projection (the runway's SecondsToLimit is NaN).
func overdueHeadroom(d map[string]any) urgencyRead {
	for _, p := range [][3]string{{"xid_age", "freeze_max_age", "a freeze maximum"},
		{"mxid_age", "mxid_freeze_max_age", "a multixact freeze maximum"}} {
		age, okAge := positiveField(d, p[0])
		limit, okLimit := positiveField(d, p[1])
		if !okAge || !okLimit {
			continue
		}
		basis := fmt.Sprintf("%s %s against %s of %s", p[0], approxCount(age), p[2],
			approxCount(limit))
		return headroom(basis, "the table is past its freeze maximum and no time "+
			"to wraparound is projected", pick(d, p[0], p[1], "fraction"))
	}
	return urgencyRead{whyNow: notAssessed}
}

// number is v as a finite float64; a string, bool, NaN or Inf is not one.
func number(v any) (float64, bool) {
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

func finiteField(d map[string]any, key string) (float64, bool) {
	v, ok := d[key]
	if !ok {
		return 0, false
	}
	return number(v)
}

func positiveField(d map[string]any, key string) (float64, bool) {
	f, ok := finiteField(d, key)
	return f, ok && f > 0
}

func stringField(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

// pick copies the named fields the producer wrote, as it wrote them.
func pick(d map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := d[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// plain is v to at most two decimals, without trailing zeros.
func plain(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// approxCount writes a large count as 1.2B, 450M or 12K.
func approxCount(v float64) string {
	for _, u := range []struct {
		div  float64
		unit string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if v >= u.div {
			return strconv.FormatFloat(math.Round(v/u.div*10)/10, 'f', -1, 64) + u.unit
		}
	}
	return plain(v)
}

// HumanDuration is secs in words: minutes under an hour, hours under two
// days, then days.
func HumanDuration(secs float64) string {
	switch {
	case secs < 3600:
		return plural(math.Max(1, math.Round(secs/60)), "minute")
	case secs < 2*86400:
		return plural(math.Round(secs/3600), "hour")
	default:
		return plural(math.Round(secs/86400), "day")
	}
}

func plural(n float64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%.0f %ss", n, unit)
}

// compactDuration is a measurement span: 45 min, 6 h or 3 d.
func compactDuration(secs float64) string {
	switch {
	case secs < 3600:
		return fmt.Sprintf("%.0f min", math.Max(1, math.Round(secs/60)))
	case secs < 2*86400:
		return fmt.Sprintf("%.0f h", math.Round(secs/3600))
	default:
		return fmt.Sprintf("%.0f d", math.Round(secs/86400))
	}
}
