package autonomy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// Dogfood lifeos-1 finding 6: "custodian proposal withheld by policy:
// blast_radius_exceeded" was logged as an ERROR every cycle. A route that
// declines (policy withheld it, it cannot be verified) is an expected
// outcome: reported once at info until it changes, never as an error.

type parkingRouter struct{ recordingRouter }

func (r *parkingRouter) Route(ctx context.Context, p Proposal) error {
	_ = r.recordingRouter.Route(ctx, p)
	return &schemaguard.ParkedRoute{Reason: "withheld by policy: blast_radius_exceeded " +
		"on public.orders"}
}

func TestDatabaseCycle_ParkedRouteIsReportedOnceAtInfo(t *testing.T) {
	ticks := make(chan time.Time, 1)
	freeze := &recordingCustodian{proposals: []Proposal{{Feature: "freeze",
		SQL: `VACUUM (FREEZE) "public"."orders"`}}}
	router := &parkingRouter{}
	cfg := workerConfig("orders", ticks, freeze, &recordingCustodian{}, router,
		&recordingAuditor{result: ledger.AuditResult{OK: true}})
	reporter := cfg.Reporter.(*recordingReporter)
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{cfg})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supervisor.Start(context.Background())
	for i := 0; i < 3; i++ {
		ticks <- time.Now()
		want := i + 1
		requireEventually(t, func() bool { return len(router.routed()) == want })
	}
	shutdownSupervisor(t, supervisor)
	var infos int
	for _, r := range reporter.snapshot() {
		if r.level == "error" {
			t.Fatalf("parked route reported as error: %+v", r)
		}
		if strings.Contains(r.message, "parked") &&
			strings.Contains(r.fields["reason"].(string), "blast_radius_exceeded") {
			infos++
		}
	}
	if infos != 1 {
		t.Fatalf("parked reports = %d over 3 cycles, want 1: %+v", infos,
			reporter.snapshot())
	}
}

// A real failure is still an error, every time.
func TestDatabaseCycle_RealRouteFailureStaysAnError(t *testing.T) {
	ticks := make(chan time.Time, 1)
	freeze := &recordingCustodian{proposals: []Proposal{{Feature: "freeze",
		SQL: `VACUUM (FREEZE) "public"."orders"`}}}
	router := &failingRouter{}
	cfg := workerConfig("orders", ticks, freeze, &recordingCustodian{}, router,
		&recordingAuditor{result: ledger.AuditResult{OK: true}})
	reporter := cfg.Reporter.(*recordingReporter)
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{cfg})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supervisor.Start(context.Background())
	ticks <- time.Now()
	requireEventually(t, func() bool { return len(reporter.snapshot()) > 0 })
	shutdownSupervisor(t, supervisor)
	if r := reporter.snapshot()[0]; r.level != "error" ||
		!strings.Contains(r.fields["error"].(string), "connection refused") {
		t.Fatalf("report = %+v, want the failure as an error", r)
	}
}

type failingRouter struct{ recordingRouter }

func (r *failingRouter) Route(context.Context, Proposal) error {
	return errConnRefused
}

var errConnRefused = &routeError{"connection refused"}

type routeError struct{ s string }

func (e *routeError) Error() string { return e.s }
