package modelpass

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/report"
)

// review-05a's G-M2: with review.model_pass auto, a model the gate dropped
// is not called; an operator's on calls it anyway; a read error keeps the
// pass (G-M2 only narrows, so failing to read it changes nothing). No
// concurrent access test: Dropped is read once per review.

func droppedFor(model string, err error) func(context.Context, string) (bool, string,
	error) {
	return func(_ context.Context, m string) (bool, string, error) {
		if err != nil {
			return false, "", err
		}
		if m == model {
			return true, "G-M2: 0 accepted steps only the model proposed in 50 reviews", nil
		}
		return false, "", nil
	}
}

func TestReview_GM2DroppedModelIsNotCalledInAuto(t *testing.T) {
	in := input(t, "db1")
	model := always(keepRanked(t, in, "t_a"))
	d := deps(model, &memStore{})
	d.Dropped = droppedFor("test-model", nil)
	rep, err := Review(context.Background(), settings(3, "majority"), d, in)
	if err != nil || rep.ModelPass != report.PassSkipped ||
		!strings.Contains(rep.ModelPassReason, "G-M2") {
		t.Fatalf("pass %s %q (%v), want skipped by G-M2", rep.ModelPass, rep.ModelPassReason,
			err)
	}
	if model.count() != 0 || len(rep.Ranked) == 0 {
		t.Fatalf("%d model calls, %d ranked: want none and the deterministic ranking",
			model.count(), len(rep.Ranked))
	}
}

func TestReview_GM2OperatorOnAndOtherModelsStillRun(t *testing.T) {
	in := input(t, "db1")
	for name, c := range map[string]struct {
		mode, dropped string
		err           error
	}{
		"operator on":    {"on", "test-model", nil},
		"another model":  {"auto", "old-model", nil},
		"read error":     {"auto", "test-model", errors.New("connection refused")},
		"no gate reader": {"auto", "", nil},
	} {
		model := always(keepRanked(t, in, "t_a"))
		d := deps(model, &memStore{})
		if c.dropped != "" {
			d.Dropped = droppedFor(c.dropped, c.err)
		}
		s := settings(3, "majority")
		s.Mode = c.mode
		rep, err := Review(context.Background(), s, d, in)
		if err != nil || rep.ModelPass != report.PassDone || model.count() != 3 {
			t.Errorf("%s: pass %s %q, %d calls (%v), want done with 3 calls", name,
				rep.ModelPass, rep.ModelPassReason, model.count(), err)
		}
	}
}
