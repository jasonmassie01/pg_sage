package runway

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// clouds-07: the disk runway's limit prefers a fresh provider capacity
// (managed-cloud telemetry), falls back to the declared
// forecaster.disk_capacity_bytes, and is otherwise unknown (NaN). The
// finding says which one it projected against.

func providerCapacity(bytes float64, source string, ok bool) func(
	context.Context) (float64, string, bool) {
	return func(context.Context) (float64, string, bool) { return bytes, source, ok }
}

func sameCapacity(a, b DiskCapacity) bool {
	return a.Source == b.Source &&
		(a.Bytes == b.Bytes || (math.IsNaN(a.Bytes) && math.IsNaN(b.Bytes)))
}

func TestCapacity_Resolution(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name     string
		declared float64
		provider func(context.Context) (float64, string, bool)
		want     DiskCapacity
	}{
		{"no provider, declared (today)", 5e10, nil, DiskCapacity{5e10, CapacityDeclared}},
		{"no provider, undeclared (today)", 0, nil, DiskCapacity{nan, ""}},
		{"fresh provider overrides declared", 5e10, providerCapacity(1e11, "", true),
			DiskCapacity{1e11, CapacityProviderTelemetry}},
		{"fresh provider, undeclared", 0, providerCapacity(1e11, "", true),
			DiskCapacity{1e11, CapacityProviderTelemetry}},
		{"stale provider falls back to declared", 5e10, providerCapacity(0, "", false),
			DiskCapacity{5e10, CapacityDeclared}},
		{"stale provider, undeclared", 0, providerCapacity(0, "", false),
			DiskCapacity{nan, ""}},
		{"unbounded, undeclared", 0, providerCapacity(0, CapacityUnbounded, false),
			DiskCapacity{nan, CapacityUnbounded}},
		{"unbounded, declared wins", 5e10, providerCapacity(0, CapacityUnbounded, false),
			DiskCapacity{5e10, CapacityDeclared}},
		{"unknown provider source is dropped", 0, providerCapacity(0, "bogus", false),
			DiskCapacity{nan, ""}},
		{"ok with zero bytes is not fresh", 5e10, providerCapacity(0, "", true),
			DiskCapacity{5e10, CapacityDeclared}},
		{"ok with negative bytes", 5e10, providerCapacity(-1, "", true),
			DiskCapacity{5e10, CapacityDeclared}},
		{"ok with NaN bytes", 5e10, providerCapacity(nan, "", true),
			DiskCapacity{5e10, CapacityDeclared}},
		{"ok with +Inf bytes", 5e10, providerCapacity(math.Inf(1), "", true),
			DiskCapacity{5e10, CapacityDeclared}},
		{"negative declared is undeclared", -1, nil, DiskCapacity{nan, ""}},
	}
	for _, tc := range cases {
		got := capacity(context.Background(),
			Options{DiskCapacityBytes: tc.declared, Capacity: tc.provider})
		if !sameCapacity(got, tc.want) {
			t.Errorf("%s: capacity = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestCapacity_PassesTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen atomic.Bool
	opts := Options{DiskCapacityBytes: 5e10, Capacity: func(c context.Context) (float64,
		string, bool) {
		seen.Store(c.Err() != nil)
		return 1e11, "", c.Err() == nil
	}}
	if got := capacity(ctx, opts); !seen.Load() ||
		!sameCapacity(got, DiskCapacity{5e10, CapacityDeclared}) {
		t.Fatalf("cancelled capacity = %+v (ctx seen %v), want declared", got, seen.Load())
	}
}

func TestBuildSamples_DiskLimitUsesResolvedCapacity(t *testing.T) {
	opts := Options{DiskCapacityBytes: 5e10, WALRetainedLimitBytes: 10 << 30}
	s := fullSnapshot()
	s.Capacity = &DiskCapacity{Bytes: 2e11, Source: CapacityProviderTelemetry}
	if got := byKey(BuildSamples(s, opts))["disk_used/cluster"].Limit; got != 2e11 {
		t.Fatalf("disk limit = %v, want the provider capacity 2e11", got)
	}
	s.Capacity = &DiskCapacity{Bytes: math.NaN(), Source: CapacityUnbounded}
	if got := byKey(BuildSamples(s, opts))["disk_used/cluster"].Limit; !math.IsNaN(got) {
		t.Fatalf("unbounded disk limit = %v, want NaN", got)
	}
	s.Capacity = nil // not resolved: the declared capacity, as before
	if got := byKey(BuildSamples(s, opts))["disk_used/cluster"].Limit; got != 5e10 {
		t.Fatalf("unresolved disk limit = %v, want the declared 5e10", got)
	}
	if got := byKey(BuildSamples(s, opts))["wal_slot/cdc"].Limit; got != 10<<30 {
		t.Fatalf("slot limit = %v, capacity must not change it", got)
	}
}

func diskTrend(limit float64) probes.RunwayTrend {
	return trend(probes.RunwayDiskUsed, probes.SubjectCluster, 12, time.Hour, 9e10, limit,
		rateFor(9e10, limit, 30*time.Hour), 0.9)
}

func evalWithCapacity(t *testing.T, tr probes.RunwayTrend, c DiskCapacity) []Runway {
	t.Helper()
	rs, _ := Evaluate(EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true,
		Capacity: c}, testOptions())
	return rs
}

func TestEvaluate_DiskFindingNamesItsCapacitySource(t *testing.T) {
	rs := evalWithCapacity(t, diskTrend(1e11),
		DiskCapacity{Bytes: 1e11, Source: CapacityProviderTelemetry})
	if len(rs) != 1 || rs[0].Detail["capacity_source"] != CapacityProviderTelemetry {
		t.Fatalf("provider disk runway = %+v", rs)
	}
	if !strings.Contains(rs[0].Title, "provider-reported disk capacity") ||
		!strings.Contains(rs[0].Recommendation, "provider-reported disk capacity") {
		t.Fatalf("provider title/recommendation = %q / %q", rs[0].Title,
			rs[0].Recommendation)
	}
	rs = evalWithCapacity(t, diskTrend(1e11), DiskCapacity{Bytes: 1e11,
		Source: CapacityDeclared})
	if len(rs) != 1 || rs[0].Detail["capacity_source"] != CapacityDeclared ||
		!strings.Contains(rs[0].Title, "the declared disk capacity") {
		t.Fatalf("declared disk runway = %+v", rs)
	}
}

// A trend whose limit is not this tick's capacity (sampled under an older
// capacity) names no source rather than a wrong one; a finding without a
// resolved capacity reads as before.
func TestEvaluate_DiskFindingSourceOnlyWhenLimitMatches(t *testing.T) {
	rs := evalWithCapacity(t, diskTrend(1e11),
		DiskCapacity{Bytes: 2e11, Source: CapacityProviderTelemetry})
	if len(rs) != 1 {
		t.Fatalf("runways = %+v", rs)
	}
	if _, ok := rs[0].Detail["capacity_source"]; ok {
		t.Fatalf("mismatched limit named a source: %+v", rs[0].Detail)
	}
	if !strings.Contains(rs[0].Title, "the declared disk capacity") {
		t.Fatalf("title = %q, want today's wording", rs[0].Title)
	}
	slot := trend(probes.RunwayWALSlot, "cdc", 12, time.Hour, 5e9, 10<<30,
		rateFor(5e9, 10<<30, 16*time.Hour), 0.95)
	rs = evalWithCapacity(t, slot, DiskCapacity{Bytes: 10 << 30,
		Source: CapacityProviderTelemetry})
	if len(rs) != 1 {
		t.Fatalf("slot runways = %+v", rs)
	}
	if _, ok := rs[0].Detail["capacity_source"]; ok {
		t.Fatalf("a slot runway named a disk capacity source: %+v", rs[0].Detail)
	}
}

// erroringRunner fails every probe, so a tick reads nothing from SQL.
type erroringRunner struct{}

func (erroringRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	return probes.Result{ProbeID: id, Status: probes.StatusError, Reason: "test",
		ObservedAt: time.Now()}
}

func (r erroringRunner) RunBackground(ctx context.Context, id probes.ID,
	a probes.Args) probes.Result {
	return r.Run(ctx, id, a)
}

func TestMonitorRead_ResolvesCapacityOncePerTick(t *testing.T) {
	var calls atomic.Int64
	opts := testOptions()
	opts.DiskCapacityBytes = 5e10
	opts.Capacity = func(context.Context) (float64, string, bool) {
		calls.Add(1)
		return 3e11, "", true
	}
	m := &Monitor{runner: erroringRunner{}, opts: opts, last: map[seriesKey]lastPoint{},
		now: time.Now, logFn: func(string, string, ...any) {}}
	snap, problems := m.read(context.Background())
	if len(problems) == 0 {
		t.Fatal("erroring probes reported no problem")
	}
	if snap.Capacity == nil || !sameCapacity(*snap.Capacity,
		DiskCapacity{3e11, CapacityProviderTelemetry}) || calls.Load() != 1 {
		t.Fatalf("snapshot capacity = %+v after %d reads", snap.Capacity, calls.Load())
	}
}
