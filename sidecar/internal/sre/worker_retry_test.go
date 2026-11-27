package sre

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06: a probe refused a slot at the queue (background sampling held
// them) is run once more when the lease leaves a whole queue wait and a
// step's margin; the second result is committed whatever it is.
// No concurrency test here: one step runs its calls in order; the
// limiter's concurrent counting is pinned in probes.

func queueRefusal(id probes.ID) probes.Result {
	return probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusError,
		Reason: "concurrency_limit", Phase: probes.PhaseQueue}
}

func leaseLeaving(d time.Duration) Lease {
	return Lease{SegmentDeadline: time.Now().Add(d)}
}

func lockStep() planStep { return planStep{calls: calls(probes.LockGraph)} }

func TestRunStep_QueueRefusalThenSuccessCommitsUsable(t *testing.T) {
	r := newScriptedRunner().script(probes.LockGraph, queueRefusal(probes.LockGraph),
		rows(probes.LockGraph, probes.Row{"waiter_pid": int64(20)}))
	c := &Coordinator{runner: r}
	out := c.runStep(context.Background(), leaseLeaving(probes.QueueWait+stepMargin+
		time.Second), lockStep())
	if len(out) != 1 || out[0].Status != probes.StatusOK || len(out[0].Rows) != 1 {
		t.Fatalf("results = %+v, want one usable result", out)
	}
	if r.calls[probes.LockGraph] != 2 {
		t.Fatalf("lock_graph runs = %d, want 2 (one retry)", r.calls[probes.LockGraph])
	}
}

func TestRunStep_TwoRefusalsCommitConcurrencyLimit(t *testing.T) {
	r := newScriptedRunner().script(probes.LockGraph, queueRefusal(probes.LockGraph))
	c := &Coordinator{runner: r}
	out := c.runStep(context.Background(), leaseLeaving(time.Hour), lockStep())
	if len(out) != 1 || !probes.QueueRefused(out[0]) || out[0].Status.Usable() {
		t.Fatalf("results = %+v, want the second refusal committed", out)
	}
	if r.calls[probes.LockGraph] != 2 {
		t.Fatalf("lock_graph runs = %d, want exactly 2", r.calls[probes.LockGraph])
	}
}

func TestRunStep_DeadlineJustUnderQueueWaitAndMarginGetsNoRetry(t *testing.T) {
	r := newScriptedRunner().script(probes.LockGraph, queueRefusal(probes.LockGraph),
		rows(probes.LockGraph, probes.Row{"waiter_pid": int64(20)}))
	c := &Coordinator{runner: r}
	out := c.runStep(context.Background(), leaseLeaving(probes.QueueWait+stepMargin-
		200*time.Millisecond), lockStep())
	if len(out) != 1 || !probes.QueueRefused(out[0]) || r.calls[probes.LockGraph] != 1 {
		t.Fatalf("results = %+v after %d runs, want the refusal and no retry", out,
			r.calls[probes.LockGraph])
	}
}

func TestRunStep_ExpiredOrZeroDeadlineGetsNoRetry(t *testing.T) {
	for name, lease := range map[string]Lease{"zero": {}, "past": leaseLeaving(-time.Minute)} {
		r := newScriptedRunner().script(probes.LockGraph, queueRefusal(probes.LockGraph),
			rows(probes.LockGraph, probes.Row{"waiter_pid": int64(20)}))
		out := (&Coordinator{runner: r}).runStep(context.Background(), lease, lockStep())
		if len(out) != 1 || !probes.QueueRefused(out[0]) || r.calls[probes.LockGraph] != 1 {
			t.Fatalf("%s deadline: results = %+v after %d runs, want no retry", name, out,
				r.calls[probes.LockGraph])
		}
	}
}

func TestRunStep_OtherFailuresAreNotRetried(t *testing.T) {
	timeout := probes.Result{ProbeID: probes.LockGraph, Status: probes.StatusError,
		Reason: "statement_timeout", Phase: probes.PhaseExecution}
	r := newScriptedRunner().script(probes.LockGraph, timeout,
		rows(probes.LockGraph, probes.Row{"waiter_pid": int64(20)}))
	out := (&Coordinator{runner: r}).runStep(context.Background(), leaseLeaving(time.Hour),
		lockStep())
	if len(out) != 1 || out[0].Reason != "statement_timeout" || r.calls[probes.LockGraph] != 1 {
		t.Fatalf("results = %+v after %d runs, want the timeout as-is", out,
			r.calls[probes.LockGraph])
	}
}

func TestRunStep_RetriesOnlyTheRefusedCall(t *testing.T) {
	r := newScriptedRunner().
		script(probes.LockGraph, rows(probes.LockGraph, probes.Row{"waiter_pid": int64(20)})).
		script(probes.PreparedXacts, queueRefusal(probes.PreparedXacts),
			rows(probes.PreparedXacts))
	st := planStep{calls: calls(probes.LockGraph, probes.PreparedXacts)}
	out := (&Coordinator{runner: r}).runStep(context.Background(), leaseLeaving(time.Hour), st)
	if len(out) != 2 || out[0].ProbeID != probes.LockGraph ||
		out[1].ProbeID != probes.PreparedXacts || !out[1].Status.Usable() {
		t.Fatalf("results = %+v, want both in plan order and usable", out)
	}
	if r.calls[probes.LockGraph] != 1 || r.calls[probes.PreparedXacts] != 2 {
		t.Fatalf("runs = %v, want lock_graph 1 and prepared_xacts 2", r.calls)
	}
}

func TestRetryQueued_NilReadIsNotCalledForUsable(t *testing.T) {
	ok := rows(probes.LockGraph, probes.Row{"waiter_pid": int64(20)})
	got := retryQueued(time.Now().Add(time.Hour), ok, func() probes.Result {
		t.Fatal("a usable result was re-read")
		return probes.Result{}
	})
	if got.Status != probes.StatusOK || len(got.Rows) != 1 {
		t.Fatalf("retryQueued = %+v, want the usable result unchanged", got)
	}
}

// callerRunner records the probe caller class of every run.
type callerRunner struct {
	mu      sync.Mutex
	callers []probes.Caller
}

func (r *callerRunner) Run(ctx context.Context, id probes.ID, _ probes.Args) probes.Result {
	r.mu.Lock()
	r.callers = append(r.callers, probes.CallerOf(ctx))
	r.mu.Unlock()
	return rows(id)
}

func TestReactiveDetector_TagsItsProbesAsDetector(t *testing.T) {
	r := &callerRunner{}
	d, err := NewReactiveDetector(r, "db", DefaultDetectorConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Triggers(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.callers) == 0 {
		t.Fatal("the detector ran no probes")
	}
	for _, c := range r.callers {
		if c != probes.CallerDetector {
			t.Fatalf("detector probe caller = %q, want detector", c)
		}
	}
}
