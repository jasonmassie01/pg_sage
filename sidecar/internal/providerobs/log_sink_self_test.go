package providerobs

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// pg_sage's own auto_explain plans are counted as their own rejection
// reason and never stored; the application's plan in the same batch is
// (perf-selfexcl).
func TestLogSinkSkipsPgSagePlans(t *testing.T) {
	sink, err := NewLogSink(nil, "postgres", sinkConfig())
	if err != nil {
		t.Fatal(err)
	}
	var stored []int64
	sink.store = func(_ context.Context, p autoexplain.ObservedPlan) error {
		stored = append(stored, p.QueryID)
		return nil
	}
	self := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
		Application: "pg_sage", Message: `duration: 1 ms plan: {"Query Text":"SELECT 1",` +
			`"Query Identifier":7,"Plan":{"Total Cost":0.01}}`}
	if err := sink.Handle(context.Background(),
		[]logwatch.LogEntry{self, observedEntry()}); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0] != 42 {
		t.Fatalf("stored plans %v, want only the application's (42)", stored)
	}
	got := sink.Rejections()
	if got["pg_sage_self"] != 1 || got["invalid_json_plan"] != 0 {
		t.Fatalf("rejections = %v, want pg_sage_self 1", got)
	}
}
