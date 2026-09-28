package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// TestApplyReportsPendingContract is the G7-B23 regression: "expanded"
// was reported while nothing would ever run the contract phase.
func TestApplyReportsPendingContract(t *testing.T) {
	fixture := newFixture()
	fixture.rehearser.result.Measurement = passingRuntimeMeasurement()
	result, err := fixture.orchestrator().Apply(context.Background(), request())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Reason != ReasonContractPending {
		t.Fatalf("Reason = %q, want %q", result.Reason, ReasonContractPending)
	}
	if len(result.PendingContractSQL) != 1 ||
		!strings.Contains(result.PendingContractSQL[0], "UNIQUE USING INDEX") {
		t.Fatalf("PendingContractSQL = %#v", result.PendingContractSQL)
	}
	if fixture.recorder.last.Reason != ReasonContractPending {
		t.Fatalf("recorded reason = %q", fixture.recorder.last.Reason)
	}
}

func passingRuntimeMeasurement() rehearsalpkg.Measurement {
	return rehearsalpkg.Measurement{AffectedQueries: []rehearsalpkg.QueryMeasurement{{
		QueryID: 1, BeforeLatencyMS: 1, AfterLatencyMS: 1,
	}}}
}

// TestApplyRecordsCleanupFailureAndMeasurement covers R08's durable
// cleanup-failure record and G7-B22's discarded measurement.
func TestApplyRecordsCleanupFailureAndMeasurement(t *testing.T) {
	fixture := newFixture()
	fixture.rehearser.result = rehearsalpkg.Result{
		Verdict: rehearsalpkg.VerdictRecommendOnly,
		Reason:  rehearsalpkg.ReasonInconclusiveWorkload,
		Measurement: rehearsalpkg.Measurement{
			MaxLockDuration: 30 * time.Millisecond, DiskDeltaBytes: 99,
		},
		CleanupError: "destroy clone clone-1: busy",
	}
	result, err := fixture.orchestrator().Apply(context.Background(), request())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Verdict != VerdictRecommendOnly || fixture.applier.calls != 0 {
		t.Fatalf("result=%#v applies=%d", result, fixture.applier.calls)
	}
	last := fixture.recorder.last
	if !strings.Contains(last.Reason, "clone_cleanup_failed") ||
		!strings.Contains(last.Reason,
			string(rehearsalpkg.ReasonInconclusiveWorkload)) {
		t.Fatalf("recorded reason = %q", last.Reason)
	}
	if last.Measurement == nil || last.Measurement.DiskDeltaBytes != 99 {
		t.Fatalf("recorded measurement = %#v", last.Measurement)
	}
}

// TestPostgresRecorderPersistsMeasurement is the G7-B22 persistence
// half: the measurement column was always written as an empty object.
func TestPostgresRecorderPersistsMeasurement(t *testing.T) {
	pool := migrationRuntimePool(t)
	record := Record{
		Request: request(), Verdict: VerdictRecommendOnly,
		EvidenceID: "ev_runtime_measurement", Reason: "inconclusive_no_workload",
		Measurement: &rehearsalpkg.Measurement{
			MaxLockDuration: 250 * time.Millisecond, DiskDeltaBytes: 8192,
		},
	}
	if err := NewPostgresRecorder(pool).Record(context.Background(), record); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var disk int64
	var lockMS float64
	var reason string
	err := pool.QueryRow(context.Background(), `SELECT
		(measurement->>'disk_delta_bytes')::bigint,
		(measurement->>'max_lock_duration_ms')::float8,
		measurement->>'reason'
		FROM sage.migration_run WHERE evidence_id=$1`,
		record.EvidenceID).Scan(&disk, &lockMS, &reason)
	if err != nil {
		t.Fatalf("read measurement: %v", err)
	}
	if disk != 8192 || lockMS != 250 || reason != "inconclusive_no_workload" {
		t.Fatalf("stored disk=%d lock_ms=%v reason=%q", disk, lockMS, reason)
	}
}
