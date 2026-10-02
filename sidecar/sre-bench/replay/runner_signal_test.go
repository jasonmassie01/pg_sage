package replay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Signal probes (evidence produced in Go outside the SQL catalog, such as
// the pooler telemetry of CHECK-04) are replayed through Signal, in
// recorded order, exactly like catalog probes. They are never served as
// catalog probes: a model naming one through Run is refused as forbidden
// tool use.

func poolerCase(t *testing.T) Case {
	t.Helper()
	c := twoSampleCase(t)
	pool := func(waiting int) []probes.Row {
		return []probes.Row{{"pooler": "pgb-1", "database": "checkout", "user": "app",
			"pool_mode": "transaction", "cl_active": 20, "cl_waiting": waiting,
			"sv_active": 20, "sv_idle": 0, "sv_used": 0, "maxwait_s": 2.5,
			"avg_wait_us": 1000}}
	}
	c.Observations = append(c.Observations,
		Observation{Probe: probes.PoolerPools, Status: probes.StatusOK, OffsetMS: 150,
			Rows: pool(30)},
		Observation{Probe: probes.PoolerPools, Status: probes.StatusOK, OffsetMS: 5150,
			Rows: pool(41)})
	if err := c.Validate(probes.Catalog()); err != nil {
		t.Fatalf("a case recording a signal probe is invalid: %v", err)
	}
	return c
}

func TestRunner_ServesRecordedSignalsInOrder(t *testing.T) {
	c := poolerCase(t)
	r := NewRunner(c, probes.Catalog())
	if got := r.Signals(); len(got) != 1 || got[0] != probes.PoolerPools {
		t.Fatalf("recorded signals = %v, want [pooler_pools]", got)
	}
	serve := r.Signal(probes.PoolerPools)
	ctx := context.Background()
	first, second, third := serve(ctx, probes.Args{}), serve(ctx, probes.Args{}),
		serve(ctx, probes.Args{})
	if first.Status != probes.StatusOK || second.Status != probes.StatusOK ||
		first.ProbeID != probes.PoolerPools {
		t.Fatalf("served %+v then %+v", first, second)
	}
	if !first.ObservedAt.Equal(c.DetectedAt.Add(150*time.Millisecond)) ||
		!second.ObservedAt.Equal(c.DetectedAt.Add(5150*time.Millisecond)) {
		t.Fatalf("observed at %s and %s", first.ObservedAt, second.ObservedAt)
	}
	ps, _, err := probes.PoolerPoolsOf(second)
	if err != nil || len(ps) != 1 || ps[0].ClientWaiting != 41 {
		t.Fatalf("second sample = %+v (%v)", ps, err)
	}
	if third.Status != probes.StatusUnsupported || third.Reason != ReasonNotRecorded {
		t.Fatalf("a third call = %+v, want unsupported not_recorded", third)
	}
	calls := r.Calls()
	if len(calls) != 3 || !calls[0].Served || calls[2].Served {
		t.Fatalf("calls = %+v", calls)
	}
	if len(r.Forbidden()) != 0 {
		t.Fatalf("serving a recorded signal was reported forbidden: %v", r.Forbidden())
	}
}

func TestRunner_SignalIsNotACatalogProbe(t *testing.T) {
	r := NewRunner(poolerCase(t), probes.Catalog())
	res := r.Run(context.Background(), probes.PoolerPools, probes.Args{})
	if res.Status != probes.StatusError || res.Reason != "unknown_probe" {
		t.Fatalf("pooler_pools through Run = %+v, want unknown_probe", res)
	}
	if f := r.Forbidden(); len(f) != 1 {
		t.Fatalf("forbidden = %v, want the refused call", f)
	}
}

// A case without the signal answers not_recorded, and a canceled call
// is refused like a catalog one.
func TestRunner_SignalEdges(t *testing.T) {
	r := NewRunner(twoSampleCase(t), probes.Catalog())
	if len(r.Signals()) != 0 {
		t.Fatalf("signals of a case without any = %v", r.Signals())
	}
	res := r.Signal(probes.PoolerPools)(context.Background(), probes.Args{})
	if res.Status != probes.StatusUnsupported || res.Reason != ReasonNotRecorded {
		t.Fatalf("unrecorded signal = %+v", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res = NewRunner(poolerCase(t), probes.Catalog()).Signal(probes.PoolerPools)(ctx,
		probes.Args{})
	if res.Status != probes.StatusError || res.Reason != "canceled" {
		t.Fatalf("canceled signal = %+v", res)
	}
	var nilRunner *Runner
	if res := nilRunner.Signal(probes.PoolerPools)(context.Background(),
		probes.Args{}); res.Status != probes.StatusError {
		t.Fatalf("nil runner signal = %+v", res)
	}
}

// Concurrent signal and catalog calls each get a distinct recorded
// observation (run with -race).
func TestRunner_SignalConcurrentCalls(t *testing.T) {
	r := NewRunner(poolerCase(t), probes.Catalog())
	var wg sync.WaitGroup
	results := make([]probes.Result, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.Signal(probes.PoolerPools)(context.Background(), probes.Args{})
		}(i)
	}
	wg.Wait()
	if results[0].ObservedAt.Equal(results[1].ObservedAt) ||
		results[0].Status != probes.StatusOK || results[1].Status != probes.StatusOK {
		t.Fatalf("concurrent signals = %+v", results)
	}
}
