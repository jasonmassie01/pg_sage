// Package assert provides non-fatal test assertions on stdlib testing
// only. It mirrors the subset of the retired testify/assert API this
// repo uses; every assertion reports the failure and returns whether it
// passed, so tests can guard follow-up checks.
package assert

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/check"
)

func report(t testing.TB, ok bool, detail string, msgAndArgs []any) bool {
	t.Helper()
	if !ok {
		t.Errorf("%s%s", check.Message(msgAndArgs), detail)
	}
	return ok
}

func Equal(t testing.TB, expected, actual any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Equal(expected, actual)
	return report(t, ok, detail, msgAndArgs)
}

func NoError(t testing.TB, err error, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.NoError(err)
	return report(t, ok, detail, msgAndArgs)
}

func Nil(t testing.TB, value any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Nil(value)
	return report(t, ok, detail, msgAndArgs)
}

func True(t testing.TB, value bool, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.True(value)
	return report(t, ok, detail, msgAndArgs)
}

func Truef(t testing.TB, value bool, msg string, args ...any) bool {
	t.Helper()
	ok, detail := check.True(value)
	return report(t, ok, detail, append([]any{msg}, args...))
}

func False(t testing.TB, value bool, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.False(value)
	return report(t, ok, detail, msgAndArgs)
}

func Falsef(t testing.TB, value bool, msg string, args ...any) bool {
	t.Helper()
	ok, detail := check.False(value)
	return report(t, ok, detail, append([]any{msg}, args...))
}

func Len(t testing.TB, object any, length int, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Len(object, length)
	return report(t, ok, detail, msgAndArgs)
}

func Empty(t testing.TB, object any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Empty(object)
	return report(t, ok, detail, msgAndArgs)
}

func NotEmpty(t testing.TB, object any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.NotEmpty(object)
	return report(t, ok, detail, msgAndArgs)
}

func Contains(t testing.TB, container, element any,
	msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Contains(container, element)
	return report(t, ok, detail, msgAndArgs)
}

func NotContains(t testing.TB, container, element any,
	msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.NotContains(container, element)
	return report(t, ok, detail, msgAndArgs)
}

func Greater(t testing.TB, a, b any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Greater(a, b)
	return report(t, ok, detail, msgAndArgs)
}

func GreaterOrEqual(t testing.TB, a, b any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.GreaterOrEqual(a, b)
	return report(t, ok, detail, msgAndArgs)
}

func Less(t testing.TB, a, b any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.Less(a, b)
	return report(t, ok, detail, msgAndArgs)
}

func LessOrEqual(t testing.TB, a, b any, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.LessOrEqual(a, b)
	return report(t, ok, detail, msgAndArgs)
}

func InDelta(t testing.TB, expected, actual any, delta float64,
	msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.InDelta(expected, actual, delta)
	return report(t, ok, detail, msgAndArgs)
}

func WithinDuration(t testing.TB, expected, actual time.Time,
	delta time.Duration, msgAndArgs ...any) bool {
	t.Helper()
	ok, detail := check.WithinDuration(expected, actual, delta)
	return report(t, ok, detail, msgAndArgs)
}
