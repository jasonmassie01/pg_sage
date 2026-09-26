package advisor

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// memoryUnitBytes are PostgreSQL's memory unit suffixes. They are
// case-sensitive: PG rejects "mb" or "Mb".
var memoryUnitBytes = map[string]float64{
	"B": 1, "kB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40,
}

// baseUnitBytes maps a pg_settings memory base unit to bytes.
var baseUnitBytes = map[string]float64{
	"": 1, "B": 1, "kB": 1 << 10, "8kB": 8 << 10, "MB": 1 << 20,
}

// timeUnitMs are PostgreSQL's time unit suffixes in milliseconds.
var timeUnitMs = map[string]float64{
	"us": 0.001, "ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000,
}

// parseGUCValue parses a GUC value into a comparable number: bytes for
// memory, milliseconds for time, the plain number otherwise. A value
// without a unit is interpreted in the GUC's base unit (G3-B01).
func parseGUCValue(value string, doc GUCDoc) (float64, error) {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `'"`))
	switch doc.Unit {
	case "bytes":
		base, ok := baseUnitBytes[doc.BaseUnit]
		if !ok {
			return 0, fmt.Errorf("unknown base unit %q", doc.BaseUnit)
		}
		return parseMemory(s, base)
	case "ms":
		return parseWithUnit(s, 1, timeUnitMs)
	default:
		return strconv.ParseFloat(s, 64)
	}
}

// parseMemory parses a PG memory value; a bare number is multiplied by
// baseBytes (the GUC's base unit in bytes).
func parseMemory(s string, baseBytes float64) (float64, error) {
	return parseWithUnit(s, baseBytes, memoryUnitBytes)
}

// parseWithUnit splits "<number>[ ]<unit>" and scales by the unit, or by
// base when no unit is given. Unknown (or wrongly cased) units are errors.
func parseWithUnit(
	s string, base float64, units map[string]float64,
) (float64, error) {
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && unicode.IsLetter(rune(s[i-1])) {
		i--
	}
	num, unit := strings.TrimSpace(s[:i]), s[i:]
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, err
	}
	if unit == "" {
		return n * base, nil
	}
	mult, ok := units[unit]
	if !ok {
		return 0, fmt.Errorf("invalid unit %q (units are case-sensitive)", unit)
	}
	return n * mult, nil
}
