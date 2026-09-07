// Package check implements the comparison logic behind the stdlib-only
// require and assert test helpers. Each function returns ok plus a
// human-readable detail for the failure message; the wrappers own the
// testing.TB interaction so failures point at the test call site.
package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Message renders trailing msgAndArgs the way testify did: a lone value,
// or a format string followed by its arguments.
func Message(msgAndArgs []any) string {
	switch len(msgAndArgs) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%v: ", msgAndArgs[0])
	default:
		if format, ok := msgAndArgs[0].(string); ok {
			return fmt.Sprintf(format, msgAndArgs[1:]...) + ": "
		}
		return fmt.Sprint(msgAndArgs...) + ": "
	}
}

func Equal(expected, actual any) (bool, string) {
	if objectsEqual(expected, actual) {
		return true, ""
	}
	return false, fmt.Sprintf(
		"not equal:\nexpected: %#v\nactual:   %#v", expected, actual)
}

func NotEqual(expected, actual any) (bool, string) {
	if !objectsEqual(expected, actual) {
		return true, ""
	}
	return false, fmt.Sprintf("should not be equal: %#v", actual)
}

func objectsEqual(expected, actual any) bool {
	if expBytes, ok := expected.([]byte); ok {
		actBytes, isBytes := actual.([]byte)
		return isBytes && bytes.Equal(expBytes, actBytes)
	}
	return reflect.DeepEqual(expected, actual)
}

func NoError(err error) (bool, string) {
	if err == nil {
		return true, ""
	}
	return false, fmt.Sprintf("unexpected error: %v", err)
}

func Error(err error) (bool, string) {
	if err != nil {
		return true, ""
	}
	return false, "expected an error, got nil"
}

func ErrorIs(err, target error) (bool, string) {
	if errors.Is(err, target) {
		return true, ""
	}
	return false, fmt.Sprintf("error %v is not %v", err, target)
}

func ErrorContains(err error, substr string) (bool, string) {
	if err != nil && strings.Contains(err.Error(), substr) {
		return true, ""
	}
	return false, fmt.Sprintf("error %v does not contain %q", err, substr)
}

func Nil(value any) (bool, string) {
	if isNil(value) {
		return true, ""
	}
	return false, fmt.Sprintf("expected nil, got %#v", value)
}

func NotNil(value any) (bool, string) {
	if !isNil(value) {
		return true, ""
	}
	return false, "expected a non-nil value"
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Ptr, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func True(value bool) (bool, string) {
	if value {
		return true, ""
	}
	return false, "expected true, got false"
}

func False(value bool) (bool, string) {
	if !value {
		return true, ""
	}
	return false, "expected false, got true"
}

func Zero(value any) (bool, string) {
	if value == nil ||
		objectsEqual(value,
			reflect.Zero(reflect.TypeOf(value)).Interface()) {
		return true, ""
	}
	return false, fmt.Sprintf("expected zero value, got %#v", value)
}

func Len(object any, length int) (bool, string) {
	actual, ok := lengthOf(object)
	if !ok {
		return false, fmt.Sprintf("cannot take length of %T", object)
	}
	if actual == length {
		return true, ""
	}
	return false, fmt.Sprintf(
		"length %d, want %d (object: %#v)", actual, length, object)
}

func lengthOf(object any) (int, bool) {
	if object == nil {
		return 0, false
	}
	rv := reflect.ValueOf(object)
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map,
		reflect.Slice, reflect.String:
		return rv.Len(), true
	default:
		return 0, false
	}
}

func Empty(object any) (bool, string) {
	if isEmpty(object) {
		return true, ""
	}
	return false, fmt.Sprintf("expected empty, got %#v", object)
}

func NotEmpty(object any) (bool, string) {
	if !isEmpty(object) {
		return true, ""
	}
	return false, "expected a non-empty value"
}

func isEmpty(object any) bool {
	if object == nil {
		return true
	}
	rv := reflect.ValueOf(object)
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map,
		reflect.Slice, reflect.String:
		return rv.Len() == 0
	case reflect.Ptr:
		if rv.IsNil() {
			return true
		}
		return isEmpty(rv.Elem().Interface())
	default:
		return objectsEqual(object,
			reflect.Zero(rv.Type()).Interface())
	}
}

func Contains(container, element any) (bool, string) {
	found, ok := containsElement(container, element)
	if !ok {
		return false, fmt.Sprintf(
			"%T is not a supported container", container)
	}
	if found {
		return true, ""
	}
	return false, fmt.Sprintf("%#v does not contain %#v",
		container, element)
}

func NotContains(container, element any) (bool, string) {
	found, ok := containsElement(container, element)
	if !ok {
		return false, fmt.Sprintf(
			"%T is not a supported container", container)
	}
	if !found {
		return true, ""
	}
	return false, fmt.Sprintf("%#v should not contain %#v",
		container, element)
}

func containsElement(container, element any) (found, ok bool) {
	if text, isString := container.(string); isString {
		sub, subOK := element.(string)
		return subOK && strings.Contains(text, sub), true
	}
	rv := reflect.ValueOf(container)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if objectsEqual(rv.Index(i).Interface(), element) {
				return true, true
			}
		}
		return false, true
	case reflect.Map:
		for _, key := range rv.MapKeys() {
			if objectsEqual(key.Interface(), element) {
				return true, true
			}
		}
		return false, true
	case reflect.String:
		sub, subOK := element.(string)
		return subOK && strings.Contains(rv.String(), sub), true
	default:
		return false, false
	}
}

func JSONEq(expected, actual string) (bool, string) {
	var expectedValue, actualValue any
	if err := json.Unmarshal([]byte(expected), &expectedValue); err != nil {
		return false, fmt.Sprintf("expected is not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(actual), &actualValue); err != nil {
		return false, fmt.Sprintf("actual is not valid JSON: %v", err)
	}
	if reflect.DeepEqual(expectedValue, actualValue) {
		return true, ""
	}
	return false, fmt.Sprintf(
		"JSON not equal:\nexpected: %s\nactual:   %s", expected, actual)
}

func Same(expected, actual any) (bool, string) {
	if samePointers(expected, actual) {
		return true, ""
	}
	return false, fmt.Sprintf(
		"not the same pointer:\nexpected: %p %#v\nactual:   %p %#v",
		expected, expected, actual, actual)
}

func samePointers(expected, actual any) bool {
	expType, actType := reflect.TypeOf(expected), reflect.TypeOf(actual)
	if expType == nil || expType != actType ||
		expType.Kind() != reflect.Ptr {
		return false
	}
	return reflect.ValueOf(expected).Pointer() ==
		reflect.ValueOf(actual).Pointer()
}

func IsType(expectedType, object any) (bool, string) {
	if reflect.TypeOf(expectedType) == reflect.TypeOf(object) {
		return true, ""
	}
	return false, fmt.Sprintf("type %T, want %T", object, expectedType)
}

func WithinDuration(expected, actual time.Time,
	delta time.Duration) (bool, string) {
	diff := expected.Sub(actual)
	if diff < 0 {
		diff = -diff
	}
	if diff <= delta {
		return true, ""
	}
	return false, fmt.Sprintf("time difference %v exceeds %v", diff, delta)
}
