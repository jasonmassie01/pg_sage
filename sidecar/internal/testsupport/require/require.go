// Package require provides fatal test assertions on stdlib testing only.
// It mirrors the subset of the retired testify/require API this repo
// uses, so tests keep their exact semantics without the dependency.
package require

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/check"
)

func fail(t testing.TB, ok bool, detail string, msgAndArgs []any) {
	t.Helper()
	if !ok {
		t.Fatalf("%s%s", check.Message(msgAndArgs), detail)
	}
}

func Equal(t testing.TB, expected, actual any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Equal(expected, actual)
	fail(t, ok, detail, msgAndArgs)
}

func NotEqual(t testing.TB, expected, actual any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.NotEqual(expected, actual)
	fail(t, ok, detail, msgAndArgs)
}

func NoError(t testing.TB, err error, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.NoError(err)
	fail(t, ok, detail, msgAndArgs)
}

func Error(t testing.TB, err error, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Error(err)
	fail(t, ok, detail, msgAndArgs)
}

func ErrorIs(t testing.TB, err, target error, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.ErrorIs(err, target)
	fail(t, ok, detail, msgAndArgs)
}

func ErrorContains(t testing.TB, err error, substr string,
	msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.ErrorContains(err, substr)
	fail(t, ok, detail, msgAndArgs)
}

func Nil(t testing.TB, value any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Nil(value)
	fail(t, ok, detail, msgAndArgs)
}

func NotNil(t testing.TB, value any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.NotNil(value)
	fail(t, ok, detail, msgAndArgs)
}

func True(t testing.TB, value bool, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.True(value)
	fail(t, ok, detail, msgAndArgs)
}

func False(t testing.TB, value bool, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.False(value)
	fail(t, ok, detail, msgAndArgs)
}

func Zero(t testing.TB, value any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Zero(value)
	fail(t, ok, detail, msgAndArgs)
}

func Len(t testing.TB, object any, length int, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Len(object, length)
	fail(t, ok, detail, msgAndArgs)
}

func Empty(t testing.TB, object any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Empty(object)
	fail(t, ok, detail, msgAndArgs)
}

func NotEmpty(t testing.TB, object any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.NotEmpty(object)
	fail(t, ok, detail, msgAndArgs)
}

func Contains(t testing.TB, container, element any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Contains(container, element)
	fail(t, ok, detail, msgAndArgs)
}

func NotContains(t testing.TB, container, element any,
	msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.NotContains(container, element)
	fail(t, ok, detail, msgAndArgs)
}

func JSONEq(t testing.TB, expected, actual string, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.JSONEq(expected, actual)
	fail(t, ok, detail, msgAndArgs)
}

func Greater(t testing.TB, a, b any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Greater(a, b)
	fail(t, ok, detail, msgAndArgs)
}

func GreaterOrEqual(t testing.TB, a, b any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.GreaterOrEqual(a, b)
	fail(t, ok, detail, msgAndArgs)
}

func Less(t testing.TB, a, b any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Less(a, b)
	fail(t, ok, detail, msgAndArgs)
}

func LessOrEqual(t testing.TB, a, b any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.LessOrEqual(a, b)
	fail(t, ok, detail, msgAndArgs)
}

func Positive(t testing.TB, value any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Positive(value)
	fail(t, ok, detail, msgAndArgs)
}

func InDelta(t testing.TB, expected, actual any, delta float64,
	msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.InDelta(expected, actual, delta)
	fail(t, ok, detail, msgAndArgs)
}

func Same(t testing.TB, expected, actual any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.Same(expected, actual)
	fail(t, ok, detail, msgAndArgs)
}

func IsType(t testing.TB, expectedType, object any, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.IsType(expectedType, object)
	fail(t, ok, detail, msgAndArgs)
}

func WithinDuration(t testing.TB, expected, actual time.Time,
	delta time.Duration, msgAndArgs ...any) {
	t.Helper()
	ok, detail := check.WithinDuration(expected, actual, delta)
	fail(t, ok, detail, msgAndArgs)
}

// Failf fails the test immediately with a title and formatted detail.
func Failf(t testing.TB, failureMessage, msg string, args ...any) {
	t.Helper()
	t.Fatalf("%s: %s", failureMessage, fmt.Sprintf(msg, args...))
}
