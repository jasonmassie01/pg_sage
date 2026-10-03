package autonomy

import (
	"context"
	"testing"
	"time"
)

func debounceSupervisor(
	t *testing.T, guard SchemaGuard, debounce time.Duration, ticks chan time.Time,
) *Supervisor {
	t.Helper()
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Tick: ticks, DDLDebounce: debounce,
		Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Schema: guard, Router: &recordingRouter{},
		Auditor: &recordingAuditor{}, Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	return supervisor
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatalf("condition not satisfied within %v", timeout)
	}
}

func requestN(t *testing.T, supervisor *Supervisor, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := supervisor.RequestSchemaGuard("orders"); err != nil {
			t.Fatalf("RequestSchemaGuard: %v", err)
		}
	}
}

func TestDDLBurstIsCoalescedIntoOneDebouncedRun(t *testing.T) {
	guard := &recordingSchemaGuard{}
	supervisor := debounceSupervisor(t, guard, 300*time.Millisecond, make(chan time.Time))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	requestN(t, supervisor, 1)
	waitFor(t, time.Second, func() bool { return guard.callCount() == 1 })
	requestN(t, supervisor, 50)
	time.Sleep(100 * time.Millisecond)
	if got := guard.callCount(); got != 1 {
		t.Fatalf("scans during the debounce window = %d, want 1", got)
	}
	waitFor(t, time.Second, func() bool { return guard.callCount() == 2 })
	time.Sleep(500 * time.Millisecond)
	if got := guard.callCount(); got != 2 {
		t.Fatalf("scans after a 50-DDL burst = %d, want exactly 2", got)
	}
	shutdownSupervisor(t, supervisor)
}

func TestFirstDDLRequestRunsImmediately(t *testing.T) {
	guard := &recordingSchemaGuard{}
	supervisor := debounceSupervisor(t, guard, time.Hour, make(chan time.Time))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	requestN(t, supervisor, 3)
	waitFor(t, time.Second, func() bool { return guard.callCount() == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := guard.callCount(); got != 1 {
		t.Fatalf("scans = %d, want 1 (the rest wait for the hour-long debounce)", got)
	}
	shutdownSupervisor(t, supervisor)
}

func TestPeriodicScanSatisfiesAPendingDDLRequest(t *testing.T) {
	guard := &recordingSchemaGuard{}
	ticks := make(chan time.Time, 1)
	supervisor := debounceSupervisor(t, guard, 300*time.Millisecond, ticks)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	requestN(t, supervisor, 1)
	waitFor(t, time.Second, func() bool { return guard.callCount() == 1 })
	requestN(t, supervisor, 1)
	ticks <- time.Now()
	waitFor(t, time.Second, func() bool { return guard.callCount() == 2 })
	time.Sleep(600 * time.Millisecond)
	if got := guard.callCount(); got != 2 {
		t.Fatalf("scans = %d, want 2: the periodic scan already covered the DDL", got)
	}
	shutdownSupervisor(t, supervisor)
}

func TestShutdownDropsAPendingDebouncedRun(t *testing.T) {
	guard := &recordingSchemaGuard{}
	supervisor := debounceSupervisor(t, guard, 200*time.Millisecond, make(chan time.Time))
	ctx, cancel := context.WithCancel(context.Background())
	supervisor.Start(ctx)
	requestN(t, supervisor, 1)
	waitFor(t, time.Second, func() bool { return guard.callCount() == 1 })
	requestN(t, supervisor, 1)
	cancel()
	shutdownSupervisor(t, supervisor)
	time.Sleep(400 * time.Millisecond)
	if got := guard.callCount(); got != 1 {
		t.Fatalf("scans after shutdown = %d, want 1", got)
	}
}

func TestDDLDebounceDefaultsWhenUnset(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		0:                      DefaultDDLDebounce,
		-time.Second:           DefaultDDLDebounce,
		time.Millisecond:       time.Millisecond,
		5 * time.Minute:        5 * time.Minute,
		DefaultDDLDebounce + 1: DefaultDDLDebounce + 1,
	}
	for configured, want := range cases {
		if got := ddlDebounce(DatabaseWorkersConfig{DDLDebounce: configured}); got != want {
			t.Errorf("ddlDebounce(%v) = %v, want %v", configured, got, want)
		}
	}
	if DefaultDDLDebounce != time.Minute {
		t.Fatalf("DefaultDDLDebounce = %v, want 1m", DefaultDDLDebounce)
	}
}

// TriggerSchemaGuard is an explicit, synchronous request (preflight and
// operators): it is never debounced.
func TestExplicitTriggerIsNotDebounced(t *testing.T) {
	guard := &recordingSchemaGuard{}
	supervisor := debounceSupervisor(t, guard, time.Hour, make(chan time.Time))
	for i := 0; i < 3; i++ {
		if err := supervisor.TriggerSchemaGuard(context.Background(), "orders"); err != nil {
			t.Fatalf("TriggerSchemaGuard: %v", err)
		}
	}
	if got := guard.callCount(); got != 3 {
		t.Fatalf("explicit triggers ran %d scans, want 3", got)
	}
}
