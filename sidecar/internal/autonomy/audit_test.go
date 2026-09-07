package autonomy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
)

func TestPeriodicSelfAuditLoudlyReportsLedgerViolations(t *testing.T) {
	ticks := make(chan time.Time, 1)
	auditor := &recordingAuditor{result: ledger.AuditResult{
		OK: false, Count: 2,
		Violations: []ledger.AuditViolation{
			{ActionID: 41, Kind: "missing_decision"},
			{ActionID: 42, Kind: "missing_verification"},
		},
	}}
	reporter := &recordingReporter{}
	config := workerConfig(
		"orders", ticks, &recordingCustodian{}, &recordingCustodian{},
		&recordingRouter{}, auditor,
	)
	config.Reporter = reporter
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{config})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supervisor.Start(context.Background())
	ticks <- time.Now()
	requireEventually(t, func() bool { return len(reporter.snapshot()) > 0 })
	reports := reporter.snapshot()
	if reports[0].level != "error" {
		t.Fatalf("audit report level = %q, want error", reports[0].level)
	}
	joined := reports[0].message + " " + stringifyFields(reports[0].fields)
	for _, want := range []string{"missing_decision", "missing_verification", "orders"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit report %q missing %q", joined, want)
		}
	}
	shutdownSupervisor(t, supervisor)
}

func TestPeriodicSelfAuditErrorsAreNotSwallowed(t *testing.T) {
	ticks := make(chan time.Time, 1)
	reporter := &recordingReporter{}
	config := workerConfig(
		"orders", ticks, &recordingCustodian{}, &recordingCustodian{},
		&recordingRouter{}, &recordingAuditor{err: errors.New("ledger unavailable")},
	)
	config.Reporter = reporter
	supervisor, err := NewSupervisor([]DatabaseWorkersConfig{config})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supervisor.Start(context.Background())
	ticks <- time.Now()
	requireEventually(t, func() bool { return len(reporter.snapshot()) > 0 })
	got := reporter.snapshot()[0]
	if got.level != "error" || !strings.Contains(got.message, "ledger unavailable") {
		t.Fatalf("audit failure report = %#v", got)
	}
	shutdownSupervisor(t, supervisor)
}

func stringifyFields(fields map[string]any) string {
	var result strings.Builder
	for key, value := range fields {
		result.WriteString(key)
		result.WriteString("=")
		result.WriteString(strings.TrimSpace(strings.ReplaceAll(
			strings.TrimSpace(toString(value)), "\n", " ",
		)))
	}
	return result.String()
}

func toString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if violations, ok := value.([]ledger.AuditViolation); ok {
		var result strings.Builder
		for _, violation := range violations {
			result.WriteString(violation.Kind)
		}
		return result.String()
	}
	return ""
}
