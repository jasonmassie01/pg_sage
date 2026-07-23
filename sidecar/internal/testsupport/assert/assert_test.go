package assert

import (
	"fmt"
	"testing"
	"time"
)

// recordingTB captures non-fatal failures so the assert wrappers can be
// exercised directly, including their boolean results.
type recordingTB struct {
	testing.TB
	errorsN int
	last    string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errorsN++
	r.last = fmt.Sprintf(format, args...)
}

func TestAssertReturnsResultAndContinues(t *testing.T) {
	rec := &recordingTB{}
	now := time.Now()

	passes := []bool{
		Equal(rec, "a", "a"),
		NoError(rec, nil),
		Nil(rec, nil),
		True(rec, true),
		Truef(rec, true, "always %s", "true"),
		False(rec, false),
		Falsef(rec, false, "always %s", "false"),
		Len(rec, []int{1}, 1),
		Empty(rec, ""),
		NotEmpty(rec, "x"),
		Contains(rec, "abc", "b"),
		NotContains(rec, "abc", "z"),
		Greater(rec, 2, 1),
		GreaterOrEqual(rec, 2, 2),
		Less(rec, 1, 2),
		LessOrEqual(rec, 2, 2),
		InDelta(rec, 1.0, 1.02, 0.1),
		WithinDuration(rec, now, now, time.Second),
	}
	for i, pass := range passes {
		if !pass {
			t.Errorf("passing assertion %d returned false", i)
		}
	}
	if rec.errorsN != 0 {
		t.Fatalf("passing assertions recorded %d errors, last: %s",
			rec.errorsN, rec.last)
	}

	if Equal(rec, 1, 2) {
		t.Error("failed Equal returned true")
	}
	if Len(rec, []int{}, 2) {
		t.Error("failed Len returned true")
	}
	if rec.errorsN != 2 {
		t.Fatalf("error count = %d, want 2", rec.errorsN)
	}
	// Non-fatal: execution continued to this point after failures.
}
