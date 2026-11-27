package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// mcp-06: every database runtime decides its due source-fix verdicts in
// the background, on its own pool, every 5 minutes, 50 at a time; it
// skips while the HA snapshot says the node is not a writable primary.

// recordingDecider records DecideDue calls.
type recordingDecider struct {
	mu        sync.Mutex
	limits    []int
	deadlines []time.Duration
	err       error
}

func (d *recordingDecider) DecideDue(ctx context.Context, limit int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.limits = append(d.limits, limit)
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	d.deadlines = append(d.deadlines, left)
	return len(d.limits), d.err
}

func (d *recordingDecider) calls() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.limits...)
}

func TestSourceFixVerifierCadence(t *testing.T) {
	if sourceFixVerifyInterval != 5*time.Minute {
		t.Fatalf("interval = %s, want 5m", sourceFixVerifyInterval)
	}
	if sourceFixVerifyLimit != 50 {
		t.Fatalf("limit = %d, want 50", sourceFixVerifyLimit)
	}
	if sourceFixVerifyTimeout <= 0 || sourceFixVerifyTimeout >= sourceFixVerifyInterval {
		t.Fatalf("timeout = %s, want a bound under the interval", sourceFixVerifyTimeout)
	}
}

// Happy path: a checked writable primary decides due rows with the limit
// and a bounded context.
func TestSourceFixVerifierPassOnAPrimary(t *testing.T) {
	m, _ := scriptedHAMonitor(false)
	checkHA(context.Background(), m)
	d := &recordingDecider{}
	pass := sourceFixVerifierPass("orders", d, m)
	pass(context.Background())
	pass(context.Background())
	if got := d.calls(); len(got) != 2 || got[0] != 50 || got[1] != 50 {
		t.Fatalf("DecideDue calls = %v, want two with limit 50", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, left := range d.deadlines {
		if left <= 0 || left > sourceFixVerifyTimeout {
			t.Fatalf("pass %d: deadline in %s, want within %s", i, left,
				sourceFixVerifyTimeout)
		}
	}
}

// State transitions: replica, an unknown role (never checked, or a failed
// probe) and a nil monitor all skip the pass; a promotion resumes it.
func TestSourceFixVerifierSkipsWhenNotWritable(t *testing.T) {
	d := &recordingDecider{}
	sourceFixVerifierPass("orders", d, nil)(context.Background())
	unchecked, _ := scriptedHAMonitor(false)
	sourceFixVerifierPass("orders", d, unchecked)(context.Background())
	replica, _ := scriptedHAMonitor(true)
	checkHA(context.Background(), replica)
	sourceFixVerifierPass("orders", d, replica)(context.Background())
	if got := d.calls(); len(got) != 0 {
		t.Fatalf("DecideDue ran %d times on a node that is not a writable primary", len(got))
	}
	failing, probe := scriptedHAMonitor(false)
	probe.answers = []error{errors.New("connection refused")}
	checkHA(context.Background(), failing)
	sourceFixVerifierPass("orders", d, failing)(context.Background())
	if got := d.calls(); len(got) != 0 {
		t.Fatal("a failed role probe did not skip the pass")
	}
}

// Error propagation: a failed pass is logged, not fatal; the next pass
// still runs.
func TestSourceFixVerifierPassSurvivesAnError(t *testing.T) {
	m, _ := scriptedHAMonitor(false)
	checkHA(context.Background(), m)
	d := &recordingDecider{err: errors.New("read due source-fix reports: boom")}
	pass := sourceFixVerifierPass("orders", d, m)
	pass(context.Background())
	pass(context.Background())
	if got := d.calls(); len(got) != 2 {
		t.Fatalf("DecideDue calls = %v, want 2 despite errors", got)
	}
}

// The worker runs on the runtime's worker group, notes its feature and
// stops with the runtime.
func TestStartSourceFixVerifierStopsWithTheRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders"}, ctx: ctx,
		cancel: cancel, workers: &sync.WaitGroup{}, cfg: config.DefaultConfig()}
	rt.startSourceFixVerifier()
	if len(rt.features) != 1 || rt.features[0] != "source_fix_verifier" {
		t.Fatalf("features = %v, want source_fix_verifier", rt.features)
	}
	cancel()
	done := make(chan struct{})
	go func() { rt.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the source-fix verifier did not stop with the runtime")
	}
}

// Fleet parity: every deployment mode starts one verifier per database
// runtime.
func TestSourceFixVerifierInEveryMode(t *testing.T) {
	graph := loadPackageCallGraph(t)
	want := runtimeCapability{"source-fix verifier",
		[]string{"startSourceFixVerifier", ".startSourceFixVerifier"}}
	for mode, entry := range runtimeModeEntries {
		if !want.satisfiedBy(graph.reachableCalls(entry)) {
			t.Errorf("%s runtime does not start the source-fix verifier", mode)
		}
	}
}
