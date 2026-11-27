package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/providerobs"
)

// clouds-10: Supabase memory reaches the advisor gates and Supabase
// status is served with the cloud telemetry.

const gib = int64(1) << 30

// obsAPI is a Supabase metrics endpoint with one CPU core.
type obsAPI struct {
	mu        sync.Mutex
	at        time.Time
	idle      float64
	total     int64
	available int64
}

func (a *obsAPI) Metrics(context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return fmt.Sprintf("node_time_seconds %d\n"+
		"node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} %g\n"+
		"node_memory_MemTotal_bytes %d\nnode_memory_MemAvailable_bytes %d\n",
		a.at.Unix(), a.idle, a.total, a.available), nil
}

func (a *obsAPI) Logs(context.Context, string, time.Time, time.Time) (
	[]logwatch.LogEntry, int, error,
) {
	return nil, 0, nil
}

// polledObs returns a runtime polled once (memory only) or twice (CPU
// 25%, memory), the last poll at now.
func polledObs(
	t *testing.T, now time.Time, polls int, total, available int64,
) *providerobs.Runtime {
	t.Helper()
	api := &obsAPI{at: now.Add(-time.Duration(polls-1) * time.Minute), idle: 100,
		total: total, available: available}
	obs, err := providerobs.NewRuntime(api, "postgres", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range polls {
		if i > 0 {
			api.at, api.idle = api.at.Add(time.Minute), api.idle+45
		}
		if err := obs.Poll(context.Background(), api.at); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	return obs
}

func workMemFinding(value string) analyzer.Finding {
	return analyzer.Finding{Category: "memory_tuning", Severity: "warning",
		ObjectIdentifier: "work_mem", Recommendation: "raise work_mem",
		RecommendedSQL: "ALTER SYSTEM SET work_mem = '" + value + "'"}
}

func TestSupabaseMemoryReachesAdvisorWorkMemGate(t *testing.T) {
	rt := &databaseRuntime{}
	rt.providerObs.Store(polledObs(t, time.Now(), 1, gib, gib/2))
	mem := rt.hostMemory()
	if mem.TotalBytes != gib || mem.AvailableBytes != gib/2 {
		t.Fatalf("hostMemory = %+v, want Supabase memory", mem)
	}
	// 5% of 1 GiB is 51 MiB: 128MB is refused, 32MB is kept.
	out := advisor.GateConfigFindingsHost([]analyzer.Finding{workMemFinding("128MB"),
		workMemFinding("32MB")}, mem, "supabase", "postgres", nil)
	if len(out) != 2 || out[0].RecommendedSQL != "" ||
		!strings.Contains(out[0].Recommendation, "work_mem above 5% of host memory") {
		t.Fatalf("128MB work_mem not refused: %+v", out)
	}
	// Supabase makes every server setting guidance-only, so the memory
	// gate shows only in its refusal reason.
	if strings.Contains(out[1].Recommendation, "of host memory") {
		t.Fatalf("32MB work_mem refused: %+v", out[1])
	}
	// Without memory evidence the gate cannot refuse.
	unknown := advisor.GateConfigFindingsHost([]analyzer.Finding{workMemFinding("128MB")},
		(&databaseRuntime{}).hostMemory(), "supabase", "postgres", nil)
	if len(unknown) != 1 || strings.Contains(unknown[0].Recommendation, "of host memory") {
		t.Fatalf("unknown memory refused the finding: %+v", unknown)
	}
}

func TestHostMemoryWithoutFreshSupabaseEvidence(t *testing.T) {
	if got := (&databaseRuntime{}).hostMemory(); got != (advisor.HostMemory{}) {
		t.Fatalf("no observability: %+v", got)
	}
	stale := &databaseRuntime{}
	stale.providerObs.Store(polledObs(t, time.Now().Add(-3*time.Minute), 1, gib, gib/2))
	if got := stale.hostMemory(); got != (advisor.HostMemory{}) {
		t.Fatalf("memory older than 2 min used: %+v", got)
	}
	// Managed-cloud telemetry, when the runtime has it, stays the source.
	cloud := &databaseRuntime{cloud: cloudtel.Unavailable("db", "rds", "no credentials")}
	cloud.providerObs.Store(polledObs(t, time.Now(), 1, gib, gib/2))
	if got := cloud.hostMemory(); got != (advisor.HostMemory{}) {
		t.Fatalf("cloud runtime bypassed: %+v", got)
	}
}

func TestSupabaseStatusFreshSample(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	st := newSupabaseStatus("orders", "abcdefghijklmnopqrst",
		polledObs(t, now, 2, 4*gib, gib)).Status()
	if st.Database != "orders" || st.Provider != "supabase" || !st.Available ||
		st.Reason != "" || st.Resource != "abcdefghijklmnopqrst" || st.Sample == nil {
		t.Fatalf("status = %+v", st)
	}
	s := st.Sample
	if s.Provider != "supabase" || s.CPUPct == nil || s.CPUPct.Value != 25 ||
		s.MemoryTotalBytes == nil || s.MemoryTotalBytes.Value != float64(4*gib) ||
		s.FreeableMemoryBytes == nil || s.FreeableMemoryBytes.Value != float64(gib) ||
		!s.CPUPct.At.Equal(now) || len(s.Missing) != 0 {
		t.Fatalf("sample = %+v", s)
	}
	if _, err := json.Marshal(st); err != nil {
		t.Fatalf("status does not encode: %v", err)
	}
}

func TestSupabaseStatusMemoryOnlyListsMissingCPU(t *testing.T) {
	st := newSupabaseStatus("orders", "", polledObs(t, time.Now(), 1, gib, gib/2)).Status()
	if !st.Available || st.Sample == nil || st.Sample.CPUPct != nil ||
		st.Sample.MemoryTotalBytes == nil || len(st.Sample.Missing) != 1 ||
		st.Sample.Missing[0] != "cpu_pct" {
		t.Fatalf("memory-only status = %+v sample %+v", st, st.Sample)
	}
}

func TestSupabaseStatusStaleOrEmptyIsUnavailable(t *testing.T) {
	stale := newSupabaseStatus("orders", "ref", polledObs(t, time.Now(), 2, gib, gib/2))
	stale.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	empty, err := providerobs.NewRuntime(&obsAPI{}, "postgres", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]*supabaseStatus{"stale": stale,
		"never polled": newSupabaseStatus("orders", "ref", empty)} {
		st := src.Status()
		if st.Available || st.Sample != nil || st.Provider != "supabase" ||
			!strings.HasPrefix(st.Reason, "unavailable: ") || st.Withhold == nil ||
			st.Resource != "ref" {
			t.Errorf("%s: status = %+v", name, st)
		}
	}
}

func statusFor(database string) (cloudtel.Status, bool) {
	for _, st := range cloudtel.Statuses() {
		if st.Database == database {
			return st, true
		}
	}
	return cloudtel.Status{}, false
}

func runtimeForObs(ctx context.Context, name string) *databaseRuntime {
	return &databaseRuntime{ctx: ctx, workers: &sync.WaitGroup{},
		spec: databaseRuntimeSpec{Name: name}}
}

func TestNoSupabaseTokenRegistersNothing(t *testing.T) {
	t.Setenv("SAGE_SUPABASE_OBSERVABILITY_TOKEN", "")
	var workers sync.WaitGroup
	obs, logs := startProviderObservability(t.Context(), &workers, nil,
		config.DefaultConfig(), nil, nil)
	if obs != nil || logs {
		t.Fatal("observability started without a token")
	}
	rt := runtimeForObs(t.Context(), "obs-no-token")
	rt.attachProviderObservability(obs)
	if _, ok := statusFor("obs-no-token"); ok {
		t.Fatal("status entry without Supabase observability")
	}
	if got := rt.hostMemory(); got != (advisor.HostMemory{}) {
		t.Fatalf("hostMemory = %+v without a token", got)
	}
}

func TestAttachProviderObservabilityLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt := runtimeForObs(ctx, "obs-lifecycle")
	rt.attachProviderObservability(polledObs(t, time.Now(), 1, gib, gib/2))
	st, ok := statusFor("obs-lifecycle")
	if !ok || st.Provider != "supabase" || !st.Available {
		t.Fatalf("status = %+v %v", st, ok)
	}
	if rt.hostMemory().TotalBytes != gib {
		t.Fatalf("hostMemory = %+v", rt.hostMemory())
	}
	cancel()
	rt.workers.Wait()
	if _, ok := statusFor("obs-lifecycle"); ok {
		t.Fatal("status entry outlived its runtime")
	}
}

// -race is unavailable on this machine; this still fails on a registry
// map access outside its lock.
func TestSupabaseStatusConcurrentRegistration(t *testing.T) {
	obs := polledObs(t, time.Now(), 2, gib, gib/2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			ctx, cancel := context.WithCancel(t.Context())
			rt := runtimeForObs(ctx, fmt.Sprintf("obs-race-%d", i%4))
			rt.attachProviderObservability(obs)
			cancel()
			rt.workers.Wait()
		}
	}()
	for {
		select {
		case <-done:
			for i := range 4 {
				if _, ok := statusFor(fmt.Sprintf("obs-race-%d", i)); ok {
					t.Fatalf("obs-race-%d left registered", i)
				}
			}
			return
		default:
			for _, st := range cloudtel.Statuses() {
				if strings.HasPrefix(st.Database, "obs-race-") && st.Provider != "supabase" {
					t.Fatalf("torn status %+v", st)
				}
			}
		}
	}
}
