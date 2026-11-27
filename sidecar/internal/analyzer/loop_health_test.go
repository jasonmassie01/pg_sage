package analyzer

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06: an analyzer cycle that analyzed nothing reports an
// error to the loop tracker instead of counting as a healthy run.

// No concurrent access tests here: the tracker's locking is tested in
// selfbudget, and an analyzer runs one cycle at a time.

// Nil/empty and error propagation: without a snapshot the cycle returns
// errNoSnapshot and the analyzer loop records a failed run.
func TestTrackedCycle_NoSnapshotIsALoopError(t *testing.T) {
	cfg := coverageTestConfig()
	a := New(nil, cfg, collector.New(nil, cfg, 170000, noopLog), nil, nil, nil, nil,
		noopLog)
	if err := a.cycle(context.Background()); !errors.Is(err, errNoSnapshot) {
		t.Fatalf("cycle without a snapshot = %v, want errNoSnapshot", err)
	}
	a.loops = selfbudget.NewLoops()
	a.trackedCycle(context.Background())
	s := a.loops.Snapshot()["analyzer"]
	if s.Runs != 1 || s.Errors != 1 || !s.LastSuccess.IsZero() {
		t.Fatalf("analyzer loop = %+v, want 1 run, 1 error, no last success", s)
	}
}

// The process-wide tracker stays the default: the budget breach finding
// reads it (TopLoops).
func TestNew_TracksTheProcessLoops(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	if a.loopTracker() != selfbudget.Process() {
		t.Fatal("a new analyzer does not report to the process-wide loop tracker")
	}
}
