package providerobs

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-07b: provider log sinks drain the same classifier, so they follow
// rca.deadlock_statement_text: no statement text unless it is redacted.

// No concurrent access tests: Handle and Drain serialize on the sink lock,
// covered by the rca-07 sink tests.

const sinkDeadlockDetail = "Process 11 waits for ShareLock on transaction 9; " +
	"blocked by process 12.\nProcess 12 waits for ShareLock on transaction 8; " +
	"blocked by process 11.\nProcess 11: UPDATE t SET note = 'alice@example.com' " +
	"WHERE id = 1\nProcess 12: UPDATE t SET note = 'x' WHERE id = 2"

func sinkDeadlockParticipants(t *testing.T, mode string) []rca.DeadlockParticipant {
	t.Helper()
	cfg := sinkConfig()
	cfg.RCA.DeadlockStatementText = mode
	sink, err := NewLogSink(nil, "postgres", cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(), PID: 11,
		ErrorLevel: "ERROR", SQLState: "40P01", Message: "deadlock detected",
		Detail: sinkDeadlockDetail}
	if err := sink.Handle(context.Background(), []logwatch.LogEntry{entry}); err != nil {
		t.Fatal(err)
	}
	sigs := sink.Drain()
	if len(sigs) != 1 {
		t.Fatalf("signals = %#v, want one deadlock signal", sigs)
	}
	parts, _ := sigs[0].Metrics[rca.MetricDeadlockParticipants].([]rca.DeadlockParticipant)
	if len(parts) != 2 || parts[0].PID != 11 || parts[0].BlockedBy != 12 {
		t.Fatalf("participants = %+v, want 11 and 12", parts)
	}
	return parts
}

func TestLogSinkDeadlockParticipantsFollowConfig(t *testing.T) {
	for _, p := range sinkDeadlockParticipants(t, "fingerprint") {
		if p.Statement != "" || p.Fingerprint == "" {
			t.Errorf("fingerprint mode participant = %+v", p)
		}
	}
	parts := sinkDeadlockParticipants(t, "redacted")
	if parts[0].Statement != "UPDATE t SET note = '?' WHERE id = ?" {
		t.Errorf("redacted mode statement = %q", parts[0].Statement)
	}
}
