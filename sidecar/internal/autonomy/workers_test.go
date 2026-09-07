package autonomy

import (
	"context"
	"testing"
	"time"
)

func TestSupervisorSchedulesFreezeAndWALPerDatabase(t *testing.T) {
	ticksA := make(chan time.Time, 1)
	ticksB := make(chan time.Time, 1)
	freezeA, walA := &recordingCustodian{}, &recordingCustodian{}
	freezeB, walB := &recordingCustodian{}, &recordingCustodian{}
	routerA, routerB := &recordingRouter{}, &recordingRouter{}
	auditorA, auditorB := &recordingAuditor{}, &recordingAuditor{}
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{
		workerConfig("orders", ticksA, freezeA, walA, routerA, auditorA),
		workerConfig("billing", ticksB, freezeB, walB, routerB, auditorB),
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	ticksA <- time.Now()
	ticksB <- time.Now()
	requireEventually(t, func() bool {
		return freezeA.callCount() == 1 && walA.callCount() == 1 &&
			freezeB.callCount() == 1 && walB.callCount() == 1 &&
			auditorA.callCount() == 1 && auditorB.callCount() == 1
	})
	shutdownSupervisor(t, supervisor)
}

func TestDatabaseCycleRoutesBothCustodianProposalKinds(t *testing.T) {
	ticks := make(chan time.Time, 1)
	freeze := &recordingCustodian{proposals: []Proposal{{
		Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."orders"`,
	}}}
	wal := &recordingCustodian{proposals: []Proposal{{
		Feature: "wal", SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'",
	}}}
	router := &recordingRouter{}
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{
		workerConfig("orders", ticks, freeze, wal, router, &recordingAuditor{}),
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supervisor.Start(context.Background())
	ticks <- time.Now()
	requireEventually(t, func() bool { return len(router.routed()) == 2 })
	got := router.routed()
	if got[0].Database != "orders" || got[1].Database != "orders" {
		t.Fatalf("proposal database context lost: %#v", got)
	}
	if got[0].Feature != "freeze" || got[1].Feature != "wal" {
		t.Fatalf("proposal routes = %#v, want freeze then WAL", got)
	}
	shutdownSupervisor(t, supervisor)
}

func TestSupervisorShutdownCancelsAndDrainsWorkers(t *testing.T) {
	ticks := make(chan time.Time, 1)
	freeze := &recordingCustodian{started: make(chan struct{}, 1), block: true}
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{
		workerConfig(
			"orders", ticks, freeze, &recordingCustodian{},
			&recordingRouter{}, &recordingAuditor{},
		),
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supervisor.Start(context.Background())
	ticks <- time.Now()
	<-freeze.started
	shutdownSupervisor(t, supervisor)
	if freeze.callCount() != 1 {
		t.Fatalf("freeze calls after drain = %d, want 1", freeze.callCount())
	}
}

func workerConfig(
	database string,
	ticks <-chan time.Time,
	freeze, wal Custodian,
	router ProposalRouter,
	auditor SelfAuditor,
) DatabaseWorkersConfig {
	return DatabaseWorkersConfig{
		Database: database, Tick: ticks, Freeze: freeze, WAL: wal,
		Router: router, Auditor: auditor, Reporter: &recordingReporter{},
	}
}

func requireEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not satisfied before deadline")
	}
}

func shutdownSupervisor(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
