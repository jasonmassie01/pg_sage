package providerobs

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-07: the provider sink's cycle boundary is Handle, so the repeats one
// batch deduplicated are reported as a follow-up when the next batch (even
// an empty one) arrives.
func TestLogSinkReportsRepeatsOnNextBatch(t *testing.T) {
	sink, err := NewLogSink(nil, "postgres", sinkConfig())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	entry := func(pid int) logwatch.LogEntry {
		return logwatch.LogEntry{Database: "postgres", Timestamp: at, PID: pid,
			ErrorLevel: "ERROR", SQLState: "40P01", Message: "deadlock detected"}
	}
	batch := []logwatch.LogEntry{entry(11), entry(12), entry(13)}
	if err := sink.Handle(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	first := sink.Drain()
	if len(first) != 1 || first[0].Metrics["occurrence_count"] != 1 {
		t.Fatalf("first drain = %#v, want the immediate signal", first)
	}
	if err := sink.Handle(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ups := sink.Drain()
	if len(ups) != 1 || ups[0].ID != "log_deadlock_detected" {
		t.Fatalf("follow-ups = %#v, want one deadlock follow-up", ups)
	}
	if ups[0].Metrics["occurrence_count"] != 3 ||
		!reflect.DeepEqual(ups[0].Metrics["affected_pids"], []int{11, 12, 13}) {
		t.Errorf("follow-up metrics = %v", ups[0].Metrics)
	}
}
