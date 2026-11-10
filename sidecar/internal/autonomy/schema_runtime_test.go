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
	if err := runSchemaGuardNow(ctx, supervisor, "orders"); err != nil {
		t.Fatalf("runSchemaGuardNow: %v", err)
	}
	if schemaGuard.callCount() != 2 {
		t.Fatalf("schema calls = %d, want 2", schemaGuard.callCount())
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestRequestSchemaGuardFailsClosedForUnknownDatabase(t *testing.T) {
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Schema: &recordingSchemaGuard{}, Router: &recordingRouter{},
		Auditor: &recordingAuditor{}, Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	err = supervisor.RequestSchemaGuard("missing")
	if err == nil || !strings.Contains(err.Error(), "schema guard database missing is unknown") {
		t.Fatalf("unknown database error = %v", err)
	}
}

func TestSchemaGuardScanHonorsCancellation(t *testing.T) {
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
	go func() { done <- runSchemaGuardNow(ctx, supervisor, "orders") }()
	<-guard.started
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("scan cancellation error = %v", err)
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

// A database without a schema guard refuses a DDL request instead of
// silently dropping it.
func TestRequestSchemaGuardFailsClosedWithoutSchemaGuard(t *testing.T) {
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{{
		Database: "orders", Freeze: &recordingCustodian{}, WAL: &recordingCustodian{},
		Router: &recordingRouter{}, Auditor: &recordingAuditor{},
		Reporter: &recordingReporter{},
	}})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	err = supervisor.RequestSchemaGuard("orders")
	if err == nil || !strings.Contains(err.Error(),
		"schema guard for database orders is unavailable") {
		t.Fatalf("missing schema guard error = %v", err)
	}
}
