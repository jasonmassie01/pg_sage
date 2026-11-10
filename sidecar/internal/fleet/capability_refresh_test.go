package fleet

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// fabricatedCaps is evidence a refresh with no pool must not keep.
func fabricatedCaps() ProviderCapabilities {
	return ProviderCapabilities{Provider: "postgres", ReadyForAutoSafe: true,
		Permissions: map[string]CapabilityStatus{"analyze": {Status: "ok"}},
		Extensions:  map[string]string{"pg_stat_statements": "available"}}
}

// countingExplainer counts gate consultations and executes everything.
func countingExplainer(calls *atomic.Int32) FamilyExplainer {
	return func(
		ctx context.Context, contracts []executor.ActionContract, r bool,
	) []executor.ActionPolicyDecision {
		calls.Add(1)
		return executeAnalyze(ctx, contracts, r)
	}
}

func TestRefreshWithoutPoolReplacesEvidenceWithUnknown(t *testing.T) {
	inst := &DatabaseInstance{Name: "db", Status: &InstanceStatus{Capabilities: fabricatedCaps()}}
	RefreshProviderCapabilities(t.Context(), inst, "postgres", executeAnalyze)
	caps := inst.SnapshotStatus().Capabilities
	if caps.Permissions["analyze"].Status != "unknown" ||
		caps.Extensions["pg_stat_statements"] != "unknown" || caps.ReadyForAutoSafe {
		t.Fatalf("nil-pool refresh kept fabricated evidence: %#v", caps)
	}
	if len(caps.Blockers) == 0 || caps.Provider != "postgres" {
		t.Fatalf("refresh produced provider %q blockers %v", caps.Provider, caps.Blockers)
	}
}

// The re-probe's pending provider is owned by the runtime, not by the
// evidence collection, so a refresh must keep it (and its blocker).
func TestRefreshKeepsPendingProvider(t *testing.T) {
	caps := fabricatedCaps()
	caps.PendingProvider = "rds"
	inst := &DatabaseInstance{Name: "db", Status: &InstanceStatus{Capabilities: caps}}
	RefreshProviderCapabilities(t.Context(), inst, "postgres", executeAnalyze)
	got := inst.SnapshotStatus().Capabilities
	want := "provider re-probe found rds; restart pg_sage to apply"
	if got.PendingProvider != "rds" || !containsString(got.Blockers, want) {
		t.Fatalf("pending provider lost: %q blockers %v", got.PendingProvider, got.Blockers)
	}
}

// A refresh cut short by shutdown must not overwrite the last evidence.
func TestRefreshCancelledKeepsLastEvidence(t *testing.T) {
	inst := &DatabaseInstance{Name: "db", Status: &InstanceStatus{Capabilities: fabricatedCaps()}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	RefreshProviderCapabilities(ctx, inst, "postgres", executeAnalyze)
	if got := inst.SnapshotStatus().Capabilities; got.Permissions["analyze"].Status != "ok" {
		t.Fatalf("cancelled refresh replaced evidence: %#v", got)
	}
}

func TestRefreshNilInstanceIsNoOp(t *testing.T) {
	RefreshProviderCapabilities(t.Context(), nil, "postgres", executeAnalyze)
	r := NewCapabilityRefresher(nil, "postgres", executeAnalyze, time.Millisecond)
	r.Request()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	r.Run(ctx) // must return at the deadline without panicking
	if ctx.Err() == nil {
		t.Fatal("Run returned before its context ended")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("Run returned %s after a 20ms deadline", waited)
	}
}

func TestCapabilityRefresherIntervalDefaults(t *testing.T) {
	for _, every := range []time.Duration{0, -time.Second} {
		r := NewCapabilityRefresher(nil, "postgres", nil, every)
		if r.every != DefaultCapabilityRefreshInterval {
			t.Errorf("interval %s became %s, want the default", every, r.every)
		}
	}
	if DefaultCapabilityRefreshInterval != 15*time.Minute {
		t.Fatalf("default interval = %s, want 15m", DefaultCapabilityRefreshInterval)
	}
	if r := NewCapabilityRefresher(nil, "", nil, time.Second); r.every != time.Second {
		t.Fatalf("explicit interval became %s", r.every)
	}
}

// Requests never block and coalesce while one is pending; Run serves each
// pending request once and stops with its context.
func TestCapabilityRefresherRequestCoalescesAndRunStops(t *testing.T) {
	var calls atomic.Int32
	inst := &DatabaseInstance{Name: "db", Status: &InstanceStatus{Capabilities: fabricatedCaps()}}
	r := NewCapabilityRefresher(inst, "postgres", countingExplainer(&calls), time.Hour)
	r.Request()
	r.Request() // a second request while one is pending must not block
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return calls.Load() >= 1 })
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("coalesced requests ran %d refreshes, want 1", n)
	}
	if inst.SnapshotStatus().Capabilities.Permissions["analyze"].Status != "unknown" {
		t.Fatal("requested refresh did not store new evidence")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}

func TestCapabilityRefresherTicks(t *testing.T) {
	var calls atomic.Int32
	inst := &DatabaseInstance{Name: "db", Status: &InstanceStatus{}}
	r := NewCapabilityRefresher(inst, "postgres", countingExplainer(&calls), 5*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)
	waitFor(t, func() bool { return calls.Load() >= 2 })
}

func TestManagerRequestCapabilityRefreshReachesEveryRuntime(t *testing.T) {
	var nilMgr *DatabaseManager
	nilMgr.RequestCapabilityRefresh() // a process without a fleet manager
	mgr := NewManager(&config.Config{Mode: "fleet"})
	var asked atomic.Int32
	for _, name := range []string{"a", "b"} {
		mgr.RegisterInstance(&DatabaseInstance{Name: name, Status: &InstanceStatus{},
			CapabilityRefresh: func() { asked.Add(1) }})
	}
	mgr.RegisterInstance(&DatabaseInstance{Name: "no-refresher", Status: &InstanceStatus{}})
	mgr.RequestCapabilityRefresh()
	if asked.Load() != 2 {
		t.Fatalf("refresh requested from %d runtimes, want 2", asked.Load())
	}
}

// Concurrent refreshes and readers: every snapshot is one whole refresh.
// -race is unavailable here (no cgo); this still catches map write panics.
func TestRefreshConcurrentWithFleetStatus(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := &DatabaseInstance{Name: "db", Status: &InstanceStatus{Capabilities: fabricatedCaps()}}
	mgr.RegisterInstance(inst)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				RefreshProviderCapabilities(t.Context(), inst, "postgres", executeAnalyze)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				readiness := BuildFleetReadiness("fleet", mgr.FleetStatus().Databases)
				if readiness.Databases[0].Capabilities.Extensions == nil {
					t.Error("reader saw capabilities without extensions")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
