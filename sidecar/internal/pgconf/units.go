package pgconf

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
	"B": 1, "kB": 1 << 10, "8kB": 8 << 10, "MB": 1 << 20,
}

// timeUnitMs are PostgreSQL's time unit suffixes in milliseconds.
var timeUnitMs = map[string]float64{
	"us": 0.001, "ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000,
}

// ParseValue parses a GUC value into a comparable number: bytes for
// memory, milliseconds for time, the plain number otherwise. A value
// without a unit is interpreted in the GUC's base unit (G3-B01).
func ParseValue(value string, doc GUCDoc) (float64, error) {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `'"`))
	switch doc.Unit {
	case "bytes":
		base, ok := baseUnitBytes[doc.BaseUnit]
		if !ok {
			return 0, fmt.Errorf("unknown base unit %q", doc.BaseUnit)
		}
		return parseWithUnit(s, base, memoryUnitBytes)
	case "ms":
		base := 1.0
		if doc.BaseUnit != "" {
			b, ok := timeUnitMs[doc.BaseUnit]
			if !ok {
				return 0, fmt.Errorf("unknown base unit %q", doc.BaseUnit)
			}
			base = b
		}
		return parseWithUnit(s, base, timeUnitMs)
	default:
		return strconv.ParseFloat(s, 64)
	}
}

// ToBaseUnits converts a requested value to a count of pg_settings.unit
// (the unit pg_settings.setting is expressed in), so a read-back compares
// like with like: "64MB" with unit "kB" is 65536. A unitless value is
// already in that unit.
func ToBaseUnits(value, unit string) (float64, error) {
	s := strings.TrimSpace(Unquote(value))
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	if unit == "" {
		return strconv.ParseFloat(s, 64)
	}
	if base, ok := baseUnitBytes[unit]; ok {
		bytes, err := parseWithUnit(s, base, memoryUnitBytes)
		return bytes / base, err
	}
	if base, ok := timeUnitMs[unit]; ok {
		ms, err := parseWithUnit(s, base, timeUnitMs)
		return ms / base, err
	}
	return 0, fmt.Errorf("unknown setting unit %q", unit)
}

// parseWithUnit splits "<number>[ ]<unit>" and scales by the unit, or by
// base when no unit is given. Unknown (or wrongly cased) units are errors.
func parseWithUnit(s string, base float64, units map[string]float64) (float64, error) {
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
