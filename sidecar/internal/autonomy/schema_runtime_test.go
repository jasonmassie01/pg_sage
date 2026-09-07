package autonomy

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSupervisorRunsSchemaGuardPeriodicallyAndOnDDL(t *testing.T) {
	ticks := make(chan time.Time, 1)
	schemaGuard := &recordingSchemaGuard{}
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Tick: ticks,
		Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Schema: schemaGuard, Router: &recordingRouter{},
		Auditor: &recordingAuditor{}, Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	ticks <- time.Now()
	requireEventually(t, func() bool { return schemaGuard.callCount() == 1 })
	if err := supervisor.TriggerSchemaGuard(ctx, "orders"); err != nil {
		t.Fatalf("TriggerSchemaGuard: %v", err)
	}
	if schemaGuard.callCount() != 2 {
		t.Fatalf("schema calls = %d, want 2", schemaGuard.callCount())
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestSchemaGuardTriggerFailsClosedForUnknownDatabase(t *testing.T) {
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Schema: &recordingSchemaGuard{}, Router: &recordingRouter{},
		Auditor: &recordingAuditor{}, Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	err = supervisor.TriggerSchemaGuard(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown database error = %v", err)
	}
}

func TestSchemaGuardTriggerHonorsCancellation(t *testing.T) {
	guard := &recordingSchemaGuard{started: make(chan struct{}, 1), block: true}
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Schema: guard, Router: &recordingRouter{},
		Auditor: &recordingAuditor{}, Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.TriggerSchemaGuard(ctx, "orders") }()
	<-guard.started
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("trigger cancellation error = %v", err)
	}
}

func TestRequestSchemaGuardSchedulesDDLScan(t *testing.T) {
	guard := &recordingSchemaGuard{}
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Schema: guard, Router: &recordingRouter{},
		Auditor: &recordingAuditor{}, Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	if err := supervisor.RequestSchemaGuard("orders"); err != nil {
		t.Fatalf("RequestSchemaGuard: %v", err)
	}
	requireEventually(t, func() bool { return guard.callCount() == 1 })
	shutdownSupervisor(t, supervisor)
}
