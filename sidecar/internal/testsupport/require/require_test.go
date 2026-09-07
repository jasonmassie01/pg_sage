package require

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// recordingTB captures failures instead of stopping the test, so the
// fatal wrappers can be exercised directly.
type recordingTB struct {
	testing.TB
	fatals int
	last   string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatals++
	r.last = fmt.Sprintf(format, args...)
}

func TestRequirePassAndFailPaths(t *testing.T) {
	rec := &recordingTB{}
	sentinel := errors.New("sentinel")
	now := time.Now()
	value := 1

	// Passing assertions must not record a fatal.
	Equal(rec, 1, 1)
	NotEqual(rec, 1, 2)
	NoError(rec, nil)
	Error(rec, sentinel)
	ErrorIs(rec, fmt.Errorf("wrap: %w", sentinel), sentinel)
	ErrorContains(rec, sentinel, "sent")
	Nil(rec, nil)
	NotNil(rec, value)
	True(rec, true)
	False(rec, false)
	Zero(rec, 0)
	Len(rec, []int{1, 2}, 2)
	Empty(rec, "")
	NotEmpty(rec, "x")
	Contains(rec, "abc", "b")
	NotContains(rec, "abc", "z")
	JSONEq(rec, `{"a":1}`, `{"a": 1}`)
	Greater(rec, 2, 1)
	GreaterOrEqual(rec, 2, 2)
	Less(rec, 1, 2)
	LessOrEqual(rec, 2, 2)
	Positive(rec, 1)
	InDelta(rec, 1.0, 1.01, 0.1)
	Same(rec, &value, &value)
	IsType(rec, 0, 5)
	WithinDuration(rec, now, now, time.Second)
	if rec.fatals != 0 {
		t.Fatalf("passing assertions recorded %d fatals, last: %s",
			rec.fatals, rec.last)
	}

	// Each failing assertion must record exactly one fatal.
	Equal(rec, 1, 2)
	if rec.fatals != 1 {
		t.Fatalf("failed Equal recorded %d fatals", rec.fatals)
	}
	NoError(rec, sentinel)
	True(rec, false)
	Len(rec, []int{}, 3)
	if rec.fatals != 4 {
		t.Fatalf("fatal count = %d, want 4", rec.fatals)
	}
}

func TestRequireFailfAndMessage(t *testing.T) {
	rec := &recordingTB{}
	Failf(rec, "title", "detail %d", 9)
	if rec.fatals != 1 || rec.last != "title: detail 9" {
		t.Fatalf("Failf output = %q (fatals=%d)", rec.last, rec.fatals)
	}
	rec = &recordingTB{}
	Equal(rec, 1, 2, "context %s", "info")
	if rec.fatals != 1 {
		t.Fatalf("Equal with message did not fail")
	}
	if want := "context info: "; len(rec.last) == 0 ||
		rec.last[:len(want)] != want {
		t.Fatalf("message prefix missing: %q", rec.last)
	}
}
