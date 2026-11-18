package config

import (
	"math"
	"strconv"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// Clamp renders v, a computed value of the named setting in its pgconf
// unit (bytes, milliseconds, a ratio or a count), inside the setting's
// documented safe range: rounded to the setting's granularity, then
// raised to the minimum or lowered to the maximum. NaN is the minimum.
// The result is how SHOW prints it ("8MB", "30min", "1.1"), so a landing
// check compares like with like. ok is false for a setting pgconf does
// not document: no range, so no value is ever emitted for it.
func Clamp(name string, v float64) (string, bool) {
	doc, ok := pgconf.Docs[name]
	if !ok {
		return "", false
	}
	switch {
	case math.IsNaN(v):
		v = doc.SafeMin
	case doc.Unit == "bytes" && v >= 1<<20:
		v = math.Floor(v/(1<<20)) * (1 << 20)
	case doc.Unit == "ms" && doc.BaseUnit == "s":
		v = math.Floor(v/1000) * 1000
	case doc.Unit == "ms" || doc.Unit == "count":
		v = math.Round(v)
	case doc.Unit == "ratio":
		v = math.Floor(v*1000) / 1000
	}
	v = math.Min(math.Max(v, doc.SafeMin), doc.SafeMax)
	switch doc.Unit {
	case "bytes":
		return renderBytes(v), true
	case "ms":
		return renderMillis(v), true
	}
	return strconv.FormatFloat(v, 'f', -1, 64), true
}

// renderBytes prints whole bytes in the largest unit that divides them
// exactly, as SHOW does.
func renderBytes(b float64) string {
	n := int64(b)
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"kB", 1 << 10}} {
		if n >= u.size && n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}

// renderMillis prints whole milliseconds in the largest unit that divides
// them exactly, as SHOW does.
func renderMillis(ms float64) string {
	n := int64(ms)
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"d", 86_400_000}, {"h", 3_600_000}, {"min", 60_000}, {"s", 1000}} {
		if n >= u.size && n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10) + "ms"
}

// parsed is a value Clamp rendered, back in the setting's pgconf unit.
func parsed(name, value string) float64 {
	n, err := pgconf.ParseValue(value, pgconf.Docs[name])
	if err != nil {
		return math.NaN()
	}
	return n
}
