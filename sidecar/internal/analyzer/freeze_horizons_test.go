package analyzer

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

type fakeHorizonSource struct {
	horizons []probes.FreezeHorizon
	err      error
	calls    int
}

func (s *fakeHorizonSource) FreezeHorizons(context.Context) ([]probes.FreezeHorizon, error) {
	s.calls++
	return s.horizons, s.err
}

func freezeTestAnalyzer(src FreezeHorizonSource) *Analyzer {
	a := New(nil, testConfig(), nil, nil, nil, nil, nil, func(string, string, ...any) {})
	a.eval = newCycleEval()
	a.WithFreezeHorizons(src)
	return a
}

// State transitions: each cycle replaces the horizons; a failed read
// clears them and marks wraparound_freeze unresolvable (fail closed: an
// open finding is not resolved by a read that did not happen).
func TestLoadFreezeHorizonsEachCycleAndFailsClosed(t *testing.T) {
	src := &fakeHorizonSource{horizons: []probes.FreezeHorizon{horizon("public", "a", 9, 10)}}
	a := freezeTestAnalyzer(src)
	a.loadFreezeHorizons(context.Background())
	if len(a.extras.FreezeHorizons) != 1 || a.eval.failed["wraparound_freeze"] {
		t.Fatalf("horizons %#v, failed %v", a.extras.FreezeHorizons, a.eval.failed)
	}
	src.horizons, src.err = nil, errors.New("connection refused")
	a.eval = newCycleEval()
	a.loadFreezeHorizons(context.Background())
	if a.extras.FreezeHorizons != nil || !a.eval.failed["wraparound_freeze"] || src.calls != 2 {
		t.Fatalf("after a failed read: horizons %#v, failed %v, calls %d",
			a.extras.FreezeHorizons, a.eval.failed, src.calls)
	}
}

// Without a sampler (no autonomy runtime) the rule cannot judge: the
// category is unresolvable rather than silently green.
func TestLoadFreezeHorizonsWithoutSourceIsUnresolvable(t *testing.T) {
	a := freezeTestAnalyzer(nil)
	a.loadFreezeHorizons(context.Background())
	if a.extras.FreezeHorizons != nil || !a.eval.failed["wraparound_freeze"] {
		t.Fatalf("no source: horizons %#v, failed %v", a.extras.FreezeHorizons, a.eval.failed)
	}
}
