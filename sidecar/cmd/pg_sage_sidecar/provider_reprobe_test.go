package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// reprobeScript drives a providerReprobe: scripted probe answers, recorded
// waits and rebuild requests, and a cancel once the script runs out.
type reprobeScript struct {
	mu       sync.Mutex
	answers  []probeResult
	rebuilds []string
	rebuild  []error
	waits    []time.Duration
	probes   int
	cancel   context.CancelFunc
}

type probeResult struct {
	provider string
	err      error
}

func (s *reprobeScript) worker() providerReprobe {
	return providerReprobe{
		name: "db1", min: time.Minute, max: 30 * time.Minute,
		probe: func(context.Context) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.probes++
			if len(s.answers) == 0 {
				s.cancel()
				return providerUnknown, errors.New("script exhausted")
			}
			next := s.answers[0]
			s.answers = s.answers[1:]
			return next.provider, next.err
		},
		rebuild: func(provider string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.rebuilds = append(s.rebuilds, provider)
			if len(s.rebuild) == 0 {
				return nil
			}
			err := s.rebuild[0]
			s.rebuild = s.rebuild[1:]
			return err
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			s.mu.Lock()
			s.waits = append(s.waits, d)
			s.mu.Unlock()
			return ctx.Err()
		},
	}
}

func runScript(t *testing.T, s *reprobeScript) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	done := make(chan string, 1)
	go func() { done <- s.worker().run(ctx) }()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("re-probe worker did not return")
		return ""
	}
}

func probeDown() probeResult {
	return probeResult{providerUnknown, errors.New("provider probe aurora: connection refused")}
}

// State transition: unknown, the database recovers, exactly one rebuild
// request carrying the real provider, and no further probes.
func TestProviderReprobeRequestsOneRebuildOnRecovery(t *testing.T) {
	s := &reprobeScript{answers: []probeResult{probeDown(), probeDown(), {"rds", nil}}}
	got := runScript(t, s)
	if got != "rds" {
		t.Fatalf("run = %q, want rds", got)
	}
	if len(s.rebuilds) != 1 || s.rebuilds[0] != "rds" {
		t.Fatalf("rebuild requests = %v, want exactly [rds]", s.rebuilds)
	}
	if s.probes != 3 {
		t.Fatalf("probes = %d, want 3 (none after the definite answer)", s.probes)
	}
}

// Boundary: the backoff starts at 1 m, doubles and is capped at 30 m.
func TestProviderReprobeBackoffDoublesToCap(t *testing.T) {
	s := &reprobeScript{}
	for range 7 {
		s.answers = append(s.answers, probeDown())
	}
	if got := runScript(t, s); got != "" {
		t.Fatalf("run = %q, want empty (cancelled)", got)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	if fmt.Sprint(s.waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", s.waits, want)
	}
	if len(s.rebuilds) != 0 {
		t.Fatalf("rebuild requests = %v while the provider stayed unknown", s.rebuilds)
	}
}

// An "unknown" without an error is not a definite answer either.
func TestProviderReprobeIgnoresUnknownAnswer(t *testing.T) {
	s := &reprobeScript{answers: []probeResult{{providerUnknown, nil}, {"self-managed", nil}}}
	if got := runScript(t, s); got != "self-managed" {
		t.Fatalf("run = %q, want self-managed", got)
	}
	if len(s.rebuilds) != 1 || s.rebuilds[0] != "self-managed" {
		t.Fatalf("rebuild requests = %v, want [self-managed]", s.rebuilds)
	}
}

// Error propagation: a failed rebuild keeps the unknown runtime and asks
// again after the next successful probe.
func TestProviderReprobeRetriesFailedRebuild(t *testing.T) {
	s := &reprobeScript{
		answers: []probeResult{{"cloud-sql", nil}, {"cloud-sql", nil}},
		rebuild: []error{errors.New("prepare candidate: dial: connection refused")},
	}
	if got := runScript(t, s); got != "cloud-sql" {
		t.Fatalf("run = %q, want cloud-sql", got)
	}
	if len(s.rebuilds) != 2 {
		t.Fatalf("rebuild requests = %v, want a retry after the failure", s.rebuilds)
	}
	if len(s.waits) != 2 || s.waits[1] != 2*time.Minute {
		t.Fatalf("waits = %v, want the failure to back off to 2m", s.waits)
	}
}

// A generation that is no longer current (a reload replaced it) stops the
// worker: the new generation probes for itself.
func TestProviderReprobeStopsWhenGenerationReplaced(t *testing.T) {
	s := &reprobeScript{
		answers: []probeResult{{"rds", nil}, {"rds", nil}},
		rebuild: []error{fmt.Errorf("replace: %w", fleet.ErrInstanceConflict)},
	}
	if got := runScript(t, s); got != "" {
		t.Fatalf("run = %q, want empty after a generation conflict", got)
	}
	if len(s.rebuilds) != 1 || s.probes != 1 {
		t.Fatalf("rebuilds=%v probes=%d, want one of each", s.rebuilds, s.probes)
	}
}

func TestProviderReprobeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probed := false
	w := providerReprobe{name: "db1", min: time.Minute, max: time.Hour, sleep: sleepContext,
		probe:   func(context.Context) (string, error) { probed = true; return "rds", nil },
		rebuild: func(string) error { t.Fatal("rebuild after cancel"); return nil }}
	if got := w.run(ctx); got != "" || probed {
		t.Fatalf("run = %q probed=%v, want nothing after cancel", got, probed)
	}
}

func TestProviderReprobeDefaults(t *testing.T) {
	if providerReprobeMin != time.Minute || providerReprobeMax != 30*time.Minute {
		t.Fatalf("re-probe backoff = %v..%v, want 1m..30m", providerReprobeMin, providerReprobeMax)
	}
}

func TestSleepContextHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("sleepContext slept through a cancelled context")
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepContext(1ms) = %v", err)
	}
}

func pendingRuntime(spec databaseRuntimeSpec) *databaseRuntime {
	return &databaseRuntime{spec: spec, provider: providerUnknown, inst: &fleet.DatabaseInstance{
		Name: spec.Name, Status: &fleet.InstanceStatus{Platform: providerUnknown,
			Capabilities: fleet.BuildProviderCapabilities(providerUnknown, false, nil)}}}
}

// Standalone has no single-runtime rebuild: the finding becomes a readiness
// blocker, and the running provider stays unknown (never assigned in place).
func TestProviderRebuildStandaloneMarksPending(t *testing.T) {
	rt := pendingRuntime(databaseRuntimeSpec{Scope: "startup", Name: "db1", Shared: true})
	if err := rt.rebuildForProvider("rds"); err != nil {
		t.Fatalf("rebuildForProvider: %v", err)
	}
	snap := rt.inst.SnapshotStatus()
	if snap.Capabilities.PendingProvider != "rds" {
		t.Fatalf("pending provider = %q, want rds", snap.Capabilities.PendingProvider)
	}
	if rt.provider != providerUnknown || snap.Platform != providerUnknown {
		t.Fatalf("provider = %q platform = %q, want both still unknown",
			rt.provider, snap.Platform)
	}
}

// The YAML control database holds every runtime's control pool, so it is
// not rebuilt in place either.
func TestProviderRebuildFleetControlDatabaseMarksPending(t *testing.T) {
	p, err := pgxpool.New(context.Background(), "postgres://probe@127.0.0.1:1/probe")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	defer p.Close()
	rt := pendingRuntime(databaseRuntimeSpec{Scope: "fleet", Name: "ctl", Pool: p,
		ControlPool: p})
	if err := rt.rebuildForProvider("alloydb"); err != nil {
		t.Fatalf("rebuildForProvider: %v", err)
	}
	if got := rt.inst.SnapshotStatus().Capabilities.PendingProvider; got != "alloydb" {
		t.Fatalf("pending provider = %q, want alloydb", got)
	}
}

// Invalid input: a meta-db runtime without the store, or an unknown scope,
// is an actionable error, not a silent no-op.
func TestProviderRebuildWithoutRebuildPathErrors(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	oldMeta := globalMetaState
	globalMetaState = nil
	t.Cleanup(func() { globalMetaState = oldMeta })
	fleetMgr = fleet.NewManager(reloadBaseConfig())
	for scope, want := range map[string]string{"meta-db": "meta-db store", "other": "scope"} {
		rt := pendingRuntime(databaseRuntimeSpec{Scope: scope, Name: "db1", DatabaseID: 7})
		err := rt.rebuildForProvider("rds")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("scope %s: err = %v, want one mentioning %q", scope, err, want)
		}
		if got := rt.inst.SnapshotStatus().Capabilities.PendingProvider; got != "" {
			t.Fatalf("scope %s: pending provider = %q, want empty", scope, got)
		}
	}
}

// No concurrent-access test for the worker itself: one worker per runtime
// generation, and its only shared write (the pending provider) goes through
// DatabaseInstance.UpdateStatus, whose locking fleet tests cover.
