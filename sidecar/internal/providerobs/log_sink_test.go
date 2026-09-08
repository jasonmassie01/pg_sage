package providerobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

func sinkConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.LogWatch.Enabled = true
	cfg.AutoExplain.Enabled = true
	return cfg
}

func TestLogSinkIsolatesDatabaseAndPreservesRCA(t *testing.T) {
	sink, err := NewLogSink(nil, "postgres", sinkConfig())
	if err != nil {
		t.Fatal(err)
	}
	entry := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
		ErrorLevel: "ERROR", SQLState: "40P01", Message: "deadlock detected"}
	other := entry
	other.Database = "foreign"
	if err := sink.Handle(context.Background(), []logwatch.LogEntry{other, entry, entry}); err != nil {
		t.Fatal(err)
	}
	signals := sink.Drain()
	if len(signals) != 1 || signals[0].ID != "log_deadlock_detected" {
		t.Fatalf("RCA signals %#v", signals)
	}
	if len(sink.Drain()) != 0 {
		t.Fatal("drain delivered signals twice")
	}
}

func TestLogSinkHonorsFlagsAndSanitizesStoreErrors(t *testing.T) {
	cfg := sinkConfig()
	cfg.AutoExplain.Enabled = false
	cfg.LogWatch.Enabled = false
	sink, _ := NewLogSink(nil, "postgres", cfg)
	if err := sink.Handle(context.Background(), []logwatch.LogEntry{{Database: "postgres",
		Message: "duration: 1 ms plan: malformed"}}); err != nil {
		t.Fatal(err)
	}
	if len(sink.Drain()) != 0 {
		t.Fatal("disabled feature emitted signal")
	}
	sink, _ = NewLogSink(nil, "postgres", sinkConfig())
	cause := errors.New("sensitive query payload")
	sink.store = func(context.Context, autoexplain.ObservedPlan) error { return cause }
	entry := observedEntry()
	err := sink.Handle(context.Background(), []logwatch.LogEntry{entry})
	if err == nil || !errors.Is(err, cause) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("store error boundary %v", err)
	}
	if len(sink.Drain()) != 0 {
		t.Fatal("failed batch emitted partial RCA")
	}
}

func observedEntry() logwatch.LogEntry {
	return logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
		Message: `duration: 1 ms plan: {"Query Text":"SELECT 1","Query Identifier":42,` +
			`"Plan":{"Total Cost":0.01}}`}
}

func TestLogSinkBoundsQueueAndConcurrentDrain(t *testing.T) {
	sink, _ := NewLogSink(nil, "postgres", sinkConfig())
	entries := make([]logwatch.LogEntry, maxSignalQueue+1)
	for i := range entries {
		entries[i].Database = "postgres"
	}
	if err := sink.Handle(context.Background(), entries); err == nil {
		t.Fatal("accepted oversized batch")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = sink.Drain() }()
	}
	wg.Wait()
	sink.Stop()
	if err := sink.Handle(context.Background(), nil); err == nil {
		t.Fatal("accepted after stop")
	}
	if err := sink.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Handle(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestLogSinkRejectsMissingConfigurationAndCancellation(t *testing.T) {
	if _, err := NewLogSink(nil, "", nil); err == nil {
		t.Fatal("accepted unknown database/config")
	}
	sink, _ := NewLogSink(nil, "postgres", sinkConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Handle(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
}

func TestLogSinkReportsPermanentRejectsAndContinuesHealthyRecords(t *testing.T) {
	sink, _ := NewLogSink(nil, "postgres", sinkConfig())
	stored, reported := 0, 0
	sink.store = func(context.Context, autoexplain.ObservedPlan) error { stored++; return nil }
	sink.SetRejectionReporter(func(_ string, count uint64) { reported += int(count) })
	ordinary := observedEntry()
	ordinary.Message = "duration: 1 ms statement: SELECT 'plan:'"
	malformed := observedEntry()
	malformed.Message = "duration: 1 ms plan: truncated"
	missingID := observedEntry()
	missingID.Message = `duration: 1 ms plan: {"Query Text":"SELECT 1","Plan":{"Total Cost":1}}`
	healthy := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
		ErrorLevel: "ERROR", SQLState: "40P01", Message: "deadlock detected"}
	err := sink.Handle(context.Background(), []logwatch.LogEntry{
		ordinary, malformed, missingID, observedEntry(), healthy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored != 1 || reported != 2 || len(sink.Drain()) != 1 {
		t.Fatalf("stored %d reported %d", stored, reported)
	}
	rejected := sink.Rejections()
	if rejected["invalid_json_plan"] != 1 || rejected["missing_query_identifier"] != 1 {
		t.Fatalf("rejections %#v", rejected)
	}
	rejected["invalid_json_plan"] = 99
	if sink.Rejections()["invalid_json_plan"] != 1 {
		t.Fatal("rejection map aliases sink state")
	}
}
