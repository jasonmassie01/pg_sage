package replay

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The replay runner stands in for probes.Runner: it serves a case's
// recorded observations, in recorded order, per probe. It never invents
// evidence: a probe the case did not record (or asked for more often
// than recorded) is "unsupported: not_recorded", never a healthy empty
// result. Calls outside the catalog or with invalid arguments are
// refused like the real runner and reported as forbidden tool use.

func twoSampleCase(t *testing.T) Case {
	t.Helper()
	m := validCase()
	m["family"] = "connection_pressure"
	m["gold"] = map[string]any{"root": "pool_fan_out", "rationale": "x"}
	row := func(n int) []any {
		return []any{map[string]any{"in_current_database": true,
			"application_name": "checkout", "client_addr": "10.0.0.7", "state": "idle",
			"backends": n, "waiting_on_lock": 0, "max_connections": 200,
			"reserved_connections": 3, "total_client_backends": n + 2,
			"server_started_at": "2026-09-01T00:00:00Z"}}
	}
	m["observations"] = []any{
		map[string]any{"probe": "connection_saturation", "status": "ok", "offset_ms": 100,
			"rows": row(40)},
		map[string]any{"probe": "lock_graph", "status": "empty", "offset_ms": 110},
		map[string]any{"probe": "connection_saturation", "status": "ok", "offset_ms": 5100,
			"rows": row(41)},
	}
	c, err := parseValid(t, m)
	if err != nil {
		t.Fatalf("case: %v", err)
	}
	return c
}

func TestRunner_ServesRecordedObservationsInOrder(t *testing.T) {
	c := twoSampleCase(t)
	r := NewRunner(c, probes.Catalog())
	ctx := context.Background()
	first := r.Run(ctx, probes.ConnectionSaturation, probes.Args{})
	second := r.Run(ctx, probes.ConnectionSaturation, probes.Args{})
	if first.Status != probes.StatusOK || second.Status != probes.StatusOK {
		t.Fatalf("statuses %s %s", first.Status, second.Status)
	}
	base := c.DetectedAt
	if !first.ObservedAt.Equal(base.Add(100*time.Millisecond)) ||
		!second.ObservedAt.Equal(base.Add(5100*time.Millisecond)) {
		t.Fatalf("observed_at %s %s, want detection + offset", first.ObservedAt,
			second.ObservedAt)
	}
	spec, _ := probes.Catalog().Spec(probes.ConnectionSaturation)
	if first.ProbeID != probes.ConnectionSaturation || first.Version != spec.Version {
		t.Fatalf("probe identity %s %s, want %s %s", first.ProbeID, first.Version,
			spec.ID, spec.Version)
	}
	groups, err := probes.ConnectionGroups(second)
	if err != nil || len(groups) != 1 || groups[0].Backends != 41 ||
		groups[0].Application != "checkout" {
		t.Fatalf("second sample decodes as %+v (%v)", groups, err)
	}
	if len(first.Columns) == 0 {
		t.Fatal("a served result names its columns")
	}
}

func TestRunner_ServedRowsAreCopies(t *testing.T) {
	c := twoSampleCase(t)
	r := NewRunner(c, probes.Catalog())
	res := r.Run(context.Background(), probes.ConnectionSaturation, probes.Args{})
	res.Rows[0]["backends"] = json.Number("999")
	if c.Observations[0].Rows[0]["backends"].(json.Number).String() != "40" {
		t.Fatal("a caller mutated the frozen case through a served result")
	}
}

func TestRunner_UnrecordedProbeIsUnsupportedNeverHealthy(t *testing.T) {
	r := NewRunner(twoSampleCase(t), probes.Catalog())
	ctx := context.Background()
	res := r.Run(ctx, probes.LongTransactions, probes.Args{})
	if res.Status != probes.StatusUnsupported || res.Reason != ReasonNotRecorded ||
		len(res.Rows) != 0 || res.ProbeID != probes.LongTransactions {
		t.Fatalf("unrecorded probe = %+v", res)
	}
	_ = r.Run(ctx, probes.LockGraph, probes.Args{})
	again := r.Run(ctx, probes.LockGraph, probes.Args{})
	if again.Status != probes.StatusUnsupported || again.Reason != ReasonNotRecorded {
		t.Fatalf("a probe asked for more often than recorded = %+v", again)
	}
	if got := r.Forbidden(); len(got) != 0 {
		t.Fatalf("catalog probes with valid args are not forbidden: %v", got)
	}
}

func TestRunner_RefusesForbiddenToolUse(t *testing.T) {
	r := NewRunner(twoSampleCase(t), probes.Catalog())
	ctx := context.Background()
	unknown := r.Run(ctx, probes.ID("pg_terminate_backend"), probes.Args{})
	badArgs := r.Run(ctx, probes.ConnectionSaturation, probes.Args{PID: 42})
	if unknown.Status != probes.StatusError || unknown.Reason != "unknown_probe" {
		t.Fatalf("unknown probe = %+v", unknown)
	}
	if badArgs.Status != probes.StatusError || badArgs.Reason != "invalid_args" {
		t.Fatalf("invalid args = %+v", badArgs)
	}
	got := r.Forbidden()
	if len(got) != 2 || got[0] != "tool:pg_terminate_backend: unknown_probe" ||
		got[1] != "tool:connection_saturation: invalid_args" {
		t.Fatalf("forbidden = %q", got)
	}
	// A refused call consumes no recorded observation.
	ok := r.Run(ctx, probes.ConnectionSaturation, probes.Args{})
	if ok.Status != probes.StatusOK || !ok.ObservedAt.Equal(
		r.c.DetectedAt.Add(100*time.Millisecond)) {
		t.Fatalf("first valid call after refusals = %+v", ok)
	}
	calls := r.Calls()
	if len(calls) != 3 || calls[0].Served || calls[2].Status != probes.StatusOK ||
		!calls[2].Served {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRunner_CanceledContext(t *testing.T) {
	r := NewRunner(twoSampleCase(t), probes.Catalog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.Run(ctx, probes.ConnectionSaturation, probes.Args{})
	if res.Status != probes.StatusError || res.Reason != "canceled" {
		t.Fatalf("canceled run = %+v", res)
	}
	if again := r.Run(context.Background(), probes.ConnectionSaturation,
		probes.Args{}); again.Status != probes.StatusOK {
		t.Fatalf("a canceled call consumed an observation: %+v", again)
	}
}

func TestRunner_NilAndEmpty(t *testing.T) {
	var r *Runner
	if res := r.Run(context.Background(), probes.LockGraph, probes.Args{}); res.Status !=
		probes.StatusError || res.Reason != "not_configured" {
		t.Fatalf("nil runner = %+v", res)
	}
	empty := NewRunner(Case{DetectedAt: time.Now()}, probes.Catalog())
	if res := empty.Run(context.Background(), probes.LockGraph, probes.Args{}); res.Status !=
		probes.StatusUnsupported {
		t.Fatalf("empty case = %+v", res)
	}
}

// Concurrent callers (the coordinator runs one probe at a time, but the
// runner must not hand the same recorded sample out twice).
func TestRunner_ConcurrentCallsServeEachSampleOnce(t *testing.T) {
	r := NewRunner(twoSampleCase(t), probes.Catalog())
	var wg sync.WaitGroup
	results := make(chan probes.Result, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- r.Run(context.Background(), probes.ConnectionSaturation, probes.Args{})
		}()
	}
	wg.Wait()
	close(results)
	served := map[int64]int{}
	unsupported := 0
	for res := range results {
		if res.Status == probes.StatusOK {
			served[res.ObservedAt.UnixMilli()]++
		} else {
			unsupported++
		}
	}
	if len(served) != 2 || unsupported != 6 {
		t.Fatalf("served %v, unsupported %d; want each of 2 samples once", served,
			unsupported)
	}
	for at, n := range served {
		if n != 1 {
			t.Fatalf("sample at %d served %d times", at, n)
		}
	}
}
