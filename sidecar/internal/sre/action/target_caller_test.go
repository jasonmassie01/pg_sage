package action

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06: an action's target checks share the sidecar's probe slots, so
// their probes are tagged action_target in the queue metrics.
// No concurrency test here: sampleTarget runs one probe and holds no
// shared state of its own.

type callerTargets struct {
	mu      sync.Mutex
	callers []probes.Caller
}

func (c *callerTargets) Run(ctx context.Context, id probes.ID, _ probes.Args) probes.Result {
	c.mu.Lock()
	c.callers = append(c.callers, probes.CallerOf(ctx))
	c.mu.Unlock()
	return probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusError,
		Reason: "concurrency_limit", Phase: probes.PhaseQueue}
}

func TestSampleTarget_TagsCallerActionTarget(t *testing.T) {
	r := &callerTargets{}
	a := &ActionService{targets: r}
	ctx := probes.WithCaller(context.Background(), probes.CallerInvestigation)
	_, why := a.sampleTarget(ctx, 4242, time.Now())
	if why == nil || why.Reason != ReasonTargetProbeFailed {
		t.Fatalf("ineligible = %+v, want %s for a refused target probe", why,
			ReasonTargetProbeFailed)
	}
	if len(r.callers) != 1 || r.callers[0] != probes.CallerActionTarget {
		t.Fatalf("callers = %v, want one action_target probe", r.callers)
	}
}
