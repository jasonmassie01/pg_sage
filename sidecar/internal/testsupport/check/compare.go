package check

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

func Greater(a, b any) (bool, string) {
	return ordered(a, b, ">", func(c int) bool { return c > 0 })
}

func GreaterOrEqual(a, b any) (bool, string) {
	return ordered(a, b, ">=", func(c int) bool { return c >= 0 })
}

func Less(a, b any) (bool, string) {
	return ordered(a, b, "<", func(c int) bool { return c < 0 })
}

func LessOrEqual(a, b any) (bool, string) {
	return ordered(a, b, "<=", func(c int) bool { return c <= 0 })
}

func Positive(value any) (bool, string) {
	number, ok := toFloat(value)
	if !ok {
		return false, fmt.Sprintf("%T is not numeric", value)
	}
	if number > 0 {
		return true, ""
	}
	return false, fmt.Sprintf("expected positive, got %v", value)
}

func InDelta(expected, actual any, delta float64) (bool, string) {
	expectedFloat, expOK := toFloat(expected)
	actualFloat, actOK := toFloat(actual)
	if !expOK || !actOK {
		return false, fmt.Sprintf(
			"non-numeric operands: %T, %T", expected, actual)
	}
	diff := expectedFloat - actualFloat
	if diff < 0 {
		diff = -diff
	}
	if diff <= delta {
		return true, ""
	}
	return false, fmt.Sprintf("difference %v exceeds delta %v (%v vs %v)",
		diff, delta, expected, actual)
}

func ordered(a, b any, op string,
	accept func(int) bool) (bool, string) {
	comparison, ok := compareValues(a, b)
	if !ok {
		return false, fmt.Sprintf(
			"cannot compare %T with %T", a, b)
	}
	if accept(comparison) {
		return true, ""
	}
	return false, fmt.Sprintf("%#v %s %#v is false", a, op, b)
}

// compareValues returns -1/0/1 for a<b, a==b, a>b. Both operands must
// share the same reflect kind family (ints, uints, floats, strings, or
// time.Time).
func compareValues(a, b any) (int, bool) {
	if aTime, ok := a.(time.Time); ok {
		bTime, bOK := b.(time.Time)
		if !bOK {
			return 0, false
		}
		return aTime.Compare(bTime), true
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Kind() == reflect.String && bv.Kind() == reflect.String {
		return strings.Compare(av.String(), bv.String()), true
	}
	aFloat, aOK := toFloat(a)
	bFloat, bOK := toFloat(b)
	if !aOK || !bOK {
		return 0, false
	}
	switch {
	case aFloat < bFloat:
		return -1, true
	case aFloat > bFloat:
		return 1, true
	default:
		return 0, true
	}
}

func toFloat(value any) (float64, bool) {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	default:
		return 0, false
	}
}
