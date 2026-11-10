package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/hasource"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ha"
)

// fleet-04: each database has one HA monitor. The HA loop checks it on a
// fixed cadence and the executor checks it once before each cycle; the
// proposal router, the runway advisor, earned autonomy and the fleet
// status read its snapshot and never probe.

// rolesProbe answers the role probe from a script (the last answer
// repeats) and counts calls.
type rolesProbe struct {
	mu      sync.Mutex
	answers []error // nil: an answer from roles
	roles   []bool  // true: in recovery
	calls   atomic.Int64
}

func (p *rolesProbe) probe(context.Context) (bool, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	role, err := p.roles[0], error(nil)
	if len(p.answers) > 0 {
		err = p.answers[0]
	}
	if len(p.roles) > 1 {
		p.roles = p.roles[1:]
	}
	if len(p.answers) > 1 {
		p.answers = p.answers[1:]
	}
	return role, err
}

func scriptedHAMonitor(roles ...bool) (*ha.Monitor, *rolesProbe) {
	p := &rolesProbe{roles: roles}
	return ha.NewFromProbe(p.probe, func(string, string, ...any) {}), p
}

// recordingExecutor records what each cycle was told about the role.
type recordingExecutor struct {
	mu     sync.Mutex
	cycles []bool
}

func (r *recordingExecutor) RunCycle(_ context.Context, isReplica bool) {
	r.mu.Lock()
	r.cycles = append(r.cycles, isReplica)
	r.mu.Unlock()
}

func (r *recordingExecutor) last(t *testing.T) bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cycles) == 0 {
		t.Fatal("the executor cycle did not run")
	}
	return r.cycles[len(r.cycles)-1]
}

// consumerView is what every consumer of one monitor reports.
type consumerView struct {
	executor, router, runway, earnedSafe, statusSafe bool
	earnedRole, statusRole                           string
}

func viewConsumers(t *testing.T, m *ha.Monitor) consumerView {
	t.Helper()
	ctx := context.Background()
	exec := &recordingExecutor{}
	runFleetDBCycle(ctx, fleetCycleDeps{name: "orders", ha: m, exec: exec})
	st, err := hasource.FromSnapshot(m).HAStatus(ctx)
	if err != nil && m != nil {
		t.Fatalf("earned HA source: %v", err)
	}
	status := (&fleet.DatabaseInstance{Name: "orders", HA: m}).SnapshotStatus()
	return consumerView{
		executor:   exec.last(t),
		router:     executorProposalRouter{isReplica: haReplicaGate(m)}.replica(ctx),
		runway:     newRunwayAdvisor(nil, nil, haReplicaGate(m)).isReplica(ctx),
		earnedSafe: st.SafeMode, earnedRole: st.Role,
		statusSafe: status.HASafeMode, statusRole: status.HARole,
	}
}

// State transitions: two flips seen by the shared monitor put the
// executor gate, the router and the runway advisor in safe mode together,
// and earned autonomy and the fleet status report it.
func TestHASharedMonitorPutsEveryConsumerInSafeModeTogether(t *testing.T) {
	m, _ := scriptedHAMonitor(false, true, false)
	ctx := context.Background()
	if got := viewConsumers(t, m); got.executor || got.router || got.runway ||
		got.earnedSafe || got.statusRole != string(ha.RolePrimary) {
		t.Fatalf("stable primary: %+v, want every consumer writable", got)
	}
	checkHA(ctx, m) // replica: flip 1
	checkHA(ctx, m) // primary: flip 2, safe mode
	got := viewConsumers(t, m)
	if !got.executor || !got.router || !got.runway {
		t.Fatalf("after two flips: executor=%v router=%v runway=%v, want all not writable",
			got.executor, got.router, got.runway)
	}
	if !got.earnedSafe || !got.statusSafe || got.earnedRole != earned.RolePrimary {
		t.Fatalf("after two flips: earned safe=%v role=%s, status safe=%v, want safe mode",
			got.earnedSafe, got.earnedRole, got.statusSafe)
	}
}

// Error propagation: a failing probe makes every consumer report not
// writable on the same tick.
func TestHAFailingProbeMakesEveryConsumerNotWritable(t *testing.T) {
	m, p := scriptedHAMonitor(false)
	ctx := context.Background()
	checkHA(ctx, m)
	if haNotWritable(m) {
		t.Fatal("a checked primary reads as not writable")
	}
	p.mu.Lock()
	p.answers = []error{errors.New("connection refused")}
	p.mu.Unlock()
	checkHA(ctx, m)
	if !haNotWritable(m) {
		t.Fatal("after a failed probe the snapshot reads as writable")
	}
	got := viewConsumers(t, m)
	if !got.executor || !got.router || !got.runway {
		t.Fatalf("failed probe: executor=%v router=%v runway=%v, want all not writable",
			got.executor, got.router, got.runway)
	}
	if got.earnedRole != earned.RoleUnknown || got.statusRole != string(ha.RoleUnknown) {
		t.Fatalf("failed probe: earned role %q status role %q, want unknown",
			got.earnedRole, got.statusRole)
	}
}

// Nil/empty: a database without a pool has no monitor, and every consumer
// fails closed.
func TestHANoPoolHasNoMonitorAndEveryConsumerFailsClosed(t *testing.T) {
	m := newInstanceHAMonitor(nil, nil, "orders", nil, time.Minute, time.Minute)
	if m != nil {
		t.Fatal("a database without a pool got an HA monitor")
	}
	ctx := context.Background()
	if !haNotWritable(nil) || !haReplicaGate(nil)(ctx) || !checkHA(ctx, nil) {
		t.Fatal("a missing monitor reads as writable")
	}
	if _, err := hasource.FromSnapshot(nil).HAStatus(ctx); err == nil {
		t.Fatal("earned autonomy without a monitor reported no error")
	}
	status := (&fleet.DatabaseInstance{Name: "orders"}).SnapshotStatus()
	if status.HARole != string(ha.RoleUnknown) || status.HASafeMode {
		t.Fatalf("status without a monitor: role %q safe %v, want unknown",
			status.HARole, status.HASafeMode)
	}
	exec := &recordingExecutor{}
	runFleetDBCycle(ctx, fleetCycleDeps{name: "orders", exec: exec})
	if len(exec.cycles) != 0 {
		t.Fatalf("an executor without a monitor ran %d cycles, want none", len(exec.cycles))
	}
}

// Footprint: consumers read the snapshot, so the database sees one role
// probe per loop tick and one per executor cycle, however often the
// router, the runway advisor, earned autonomy and the status read it.
func TestHAConsumersNeverProbe(t *testing.T) {
	m, p := scriptedHAMonitor(false)
	ctx := context.Background()
	for tick := 0; tick < 3; tick++ {
		checkHA(ctx, m)
		for read := 0; read < 10; read++ {
			_ = haReplicaGate(m)(ctx)
			_ = newRunwayAdvisor(nil, nil, haReplicaGate(m)).isReplica(ctx)
			_, _ = hasource.FromSnapshot(m).HAStatus(ctx)
			_ = (&fleet.DatabaseInstance{HA: m}).SnapshotStatus()
		}
	}
	if got := p.calls.Load(); got != 3 {
		t.Fatalf("3 ticks and 120 consumer reads probed %d times, want 3", got)
	}
	runFleetDBCycle(ctx, fleetCycleDeps{name: "orders", ha: m, exec: &recordingExecutor{}})
	if got := p.calls.Load(); got != 4 {
		t.Fatalf("an executor cycle probed %d times in all, want 4 (one Check before it)",
			got)
	}
}

// The executor cycle checks the role at that moment: a replica found just
// before the cycle is passed as isReplica even if the loop saw a primary.
func TestHAExecutorCycleChecksJustBeforeRunCycle(t *testing.T) {
	m, _ := scriptedHAMonitor(false, true)
	checkHA(context.Background(), m)
	exec := &recordingExecutor{}
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "orders", ha: m, exec: exec})
	if !exec.last(t) {
		t.Fatal("a replica found just before the cycle was passed as writable")
	}
	if s := m.Snapshot(); s.Role != ha.RoleReplica {
		t.Fatalf("snapshot after the cycle's check = %q, want replica", s.Role)
	}
}

// The loop checks at its cadence, bounded by the check timeout, and stops
// with the instance context.
func TestHALoopChecksAtCadenceAndStops(t *testing.T) {
	m, p := scriptedHAMonitor(false)
	m.WithCadence(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runHALoop(ctx, m)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for p.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the HA loop did not stop with its context")
	}
	if got := p.calls.Load(); got < 3 {
		t.Fatalf("the HA loop checked %d times in 5s at a 10ms cadence", got)
	}
	stopped := p.calls.Load()
	time.Sleep(30 * time.Millisecond)
	if p.calls.Load() != stopped {
		t.Fatal("the HA loop kept checking after its context ended")
	}
	runHALoop(context.Background(), nil) // a nil monitor returns at once
}

// Every check is bounded by the check timeout, and a timed-out probe
// fails closed.
func TestHACheckIsBoundedByTheTimeout(t *testing.T) {
	m := ha.NewFromProbe(func(ctx context.Context) (bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > haCheckTimeout {
			return false, nil // an unbounded probe answers primary
		}
		return false, context.DeadlineExceeded
	}, func(string, string, ...any) {})
	if haCheckTimeout != 5*time.Second {
		t.Fatalf("check timeout = %v, want 5s", haCheckTimeout)
	}
	if !checkHA(context.Background(), m) {
		t.Fatal("a probe without a deadline, or a timed-out probe, read as writable")
	}
	if m.Snapshot().Role != ha.RoleUnknown {
		t.Fatal("a timed-out probe left a known role")
	}
}

// Boundary: the cadence is ha.check_interval_seconds (default 60 s), at
// most the orchestrator interval.
func TestHACadence(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Analyzer.IntervalSeconds = 600
	if got := haCadence(cfg); got != time.Minute {
		t.Fatalf("default cadence = %v, want 60s", got)
	}
	cfg.HA.CheckIntervalSeconds = 30
	if got := haCadence(cfg); got != 30*time.Second {
		t.Fatalf("cadence with 30s = %v, want 30s", got)
	}
	cfg.HA.CheckIntervalSeconds = 120
	cfg.Analyzer.IntervalSeconds = 55
	if got, want := haCadence(cfg), orchestratorInterval(cfg); got != want ||
		want != time.Minute {
		t.Fatalf("cadence above the orchestrator interval = %v, want %v (60s)", got, want)
	}
	cfg.HA.CheckIntervalSeconds = 0
	cfg.Analyzer.IntervalSeconds = 600
	if got := haCadence(cfg); got != time.Minute {
		t.Fatalf("cadence with an unset interval = %v, want the 60s default", got)
	}
	if got := haCadence(nil); got != ha.DefaultCheckInterval {
		t.Fatalf("cadence without a config = %v, want the default", got)
	}
}

// The failover cooldown every consumer applies is earned autonomy's
// (sre.autonomy.failover_cooldown_minutes, 30 by default): one role
// change holds the router and the executor cycle until it has passed.
func TestHAFailoverCooldownIsEarnedAutonomys(t *testing.T) {
	if ha.DefaultFailoverCooldown != earned.DefaultConfig().FailoverCooldown ||
		config.DefaultConfig().SRE.Autonomy.FailoverCooldown() != ha.DefaultFailoverCooldown {
		t.Fatalf("HA default cooldown %v, earned %v, config %v: want one 30m value",
			ha.DefaultFailoverCooldown, earned.DefaultConfig().FailoverCooldown,
			config.DefaultConfig().SRE.Autonomy.FailoverCooldown())
	}
	m, _ := scriptedHAMonitor(true, false)
	m.WithFailoverCooldown(time.Nanosecond)
	checkHA(context.Background(), m) // replica
	checkHA(context.Background(), m) // primary: one flip, no safe mode
	time.Sleep(time.Millisecond)
	if haNotWritable(m) {
		t.Fatal("a primary past a 1ns cooldown reads as not writable")
	}
	m.WithFailoverCooldown(time.Hour)
	if !haNotWritable(m) || !haReplicaGate(m)(context.Background()) {
		t.Fatal("a primary one flip ago, inside a 1h cooldown, reads as writable")
	}
}
