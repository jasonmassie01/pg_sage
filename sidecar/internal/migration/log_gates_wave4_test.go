package migration

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

func TestR16MigrationLogDetectionDisabledDoesNotAnalyzeDDL(t *testing.T) {
	cfg := &config.MigrationConfig{
		Enabled: true, LogDetection: false, ActivityPolling: true,
	}
	parser := &countingSQLParser{}
	detector := newGateLogDetector(cfg, parser)

	incident := detector.ProcessLogEntry(ddlLogEntry())
	if incident != nil {
		t.Fatalf("disabled log detector returned incident: %+v", incident)
	}
	if parser.calls != 0 {
		t.Fatalf("disabled log detector analyzed %d entries, want 0", parser.calls)
	}
}

func TestR16MigrationLogDetectionWorksWhenPollingDisabled(t *testing.T) {
	cfg := &config.MigrationConfig{
		Enabled: true, LogDetection: true, ActivityPolling: false,
	}
	parser := &countingSQLParser{}
	detector := newGateLogDetector(cfg, parser)

	if incident := detector.ProcessLogEntry(ddlLogEntry()); incident != nil {
		t.Fatalf("empty analyzer unexpectedly returned incident: %+v", incident)
	}
	if parser.calls != 1 {
		t.Fatalf("enabled log detector calls = %d, want 1", parser.calls)
	}
}

func TestR16MigrationMasterSwitchDisablesLogDetection(t *testing.T) {
	cfg := &config.MigrationConfig{
		Enabled: false, LogDetection: true, ActivityPolling: true,
	}
	parser := &countingSQLParser{}
	detector := newGateLogDetector(cfg, parser)

	detector.ProcessLogEntry(ddlLogEntry())
	if parser.calls != 0 {
		t.Fatalf("master-disabled detector analyzed %d entries", parser.calls)
	}
}

func TestR16MigrationLogDetectorIgnoresNonDDL(t *testing.T) {
	cfg := &config.MigrationConfig{Enabled: true, LogDetection: true}
	parser := &countingSQLParser{}
	detector := newGateLogDetector(cfg, parser)

	detector.ProcessLogEntry(logwatch.LogEntry{Message: "SELECT 1"})
	if parser.calls != 0 {
		t.Fatalf("non-DDL reached analyzer %d times", parser.calls)
	}
}

func TestR16MigrationLogDetectorAcceptsStatementPrefix(t *testing.T) {
	cfg := &config.MigrationConfig{Enabled: true, LogDetection: true}
	parser := &countingSQLParser{}
	detector := newGateLogDetector(cfg, parser)

	detector.ProcessLogEntry(logwatch.LogEntry{
		Message: "statement: ALTER TABLE accounts ADD COLUMN note text",
	})
	if parser.calls != 1 {
		t.Fatalf("statement-prefixed DDL calls = %d, want 1", parser.calls)
	}
}

func TestR16MigrationLogDetectorDrainsSharedEntrySourceOnce(t *testing.T) {
	cfg := &config.MigrationConfig{Enabled: true, LogDetection: true}
	parser := &countingSQLParser{}
	detector := newGateLogDetector(cfg, parser)
	source := &testLogEntrySource{entries: []logwatch.LogEntry{ddlLogEntry()}}

	detector.drain(context.Background(), source)
	detector.drain(context.Background(), source)
	if parser.calls != 1 {
		t.Fatalf("shared entry analyzed %d times, want exactly once", parser.calls)
	}
}

func TestR16ActivityPollingDisabledPollOnceIsNoop(t *testing.T) {
	cases := []config.MigrationConfig{
		{Enabled: true, LogDetection: true, ActivityPolling: false},
		{Enabled: false, LogDetection: true, ActivityPolling: true},
	}
	for _, cfg := range cases {
		detector := NewDetector(nil, nil, &cfg, nopMigrationLog)
		incidents, err := detector.PollOnce(context.Background())
		if err != nil {
			t.Fatalf("disabled activity poll returned error: %v", err)
		}
		if len(incidents) != 0 {
			t.Fatalf("disabled activity poll returned %d incidents", len(incidents))
		}
	}
}

func TestR16ActivityPollingDisabledRunDoesNotAllocateLoop(t *testing.T) {
	cfg := &config.MigrationConfig{
		Enabled: true, LogDetection: true, ActivityPolling: false,
	}
	detector := NewDetector(nil, nil, cfg, nopMigrationLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		cancel()
	case <-time.After(100 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("activity_polling=false left a polling loop running")
	}
}

func TestR16ActivityPollingRunsWhenLogDetectionDisabled(t *testing.T) {
	cfg := &config.MigrationConfig{
		Enabled: true, LogDetection: false, ActivityPolling: true,
		PollIntervalSeconds: 60,
	}
	detector := NewDetector(nil, nil, cfg, nopMigrationLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("activity polling stopped because log detection was disabled")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("activity polling did not stop after cancellation")
	}
}

type countingSQLParser struct {
	calls int
}

type testLogEntrySource struct {
	entries []logwatch.LogEntry
}

func (s *testLogEntrySource) Drain() []logwatch.LogEntry {
	entries := s.entries
	s.entries = nil
	return entries
}

func (*testLogEntrySource) Stop() {}

func (p *countingSQLParser) Classify(string, int) []DDLClassification {
	p.calls++
	return nil
}

func newGateLogDetector(
	cfg *config.MigrationConfig, parser SQLParser,
) *LogDetector {
	advisor := &Advisor{
		classifier: parser, cfg: cfg, logFn: nopMigrationLog,
	}
	return NewLogDetector(advisor, nopMigrationLog)
}

func ddlLogEntry() logwatch.LogEntry {
	return logwatch.LogEntry{
		Message: "ALTER TABLE accounts ADD COLUMN note text",
	}
}

func nopMigrationLog(string, string, ...any) {}
