package logwatch

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// rca-07: Drain appends the follow-up after the drain's own signals, so a
// consumer keeping the last signal of an id sees the counted one.
func TestFileWatcher_DrainEmitsFollowUpAfterImmediate(t *testing.T) {
	dir := t.TempDir()
	data := []byte(deadlockCSVRecord + deadlockCSVRecord + deadlockCSVRecord)
	if err := os.WriteFile(filepath.Join(dir, "postgresql.csv"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{
		LogDirectory: dir, Format: "csvlog", PollIntervalMs: 100,
		DedupWindowS: 60, MaxLineLenBytes: 65536, MaxLinesPerCycle: 10000,
	}, nopLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fw.Stop()
	sigs := followUpsWithID(fw.Drain(), "log_deadlock_detected")
	if len(sigs) != 2 {
		t.Fatalf("deadlock signals = %d, want immediate + follow-up", len(sigs))
	}
	if sigs[0].Metrics["occurrence_count"] != 1 || sigs[1].Metrics["occurrence_count"] != 3 {
		t.Errorf("counts = %v then %v, want 1 then 3",
			sigs[0].Metrics["occurrence_count"], sigs[1].Metrics["occurrence_count"])
	}
	if got := sigs[1].Metrics["affected_pids"]; !reflect.DeepEqual(got, []int{5678, 5679}) {
		t.Errorf("affected_pids = %v, want [5678 5679]", got)
	}
	if again := followUpsWithID(fw.Drain(), "log_deadlock_detected"); len(again) != 0 {
		t.Errorf("second drain re-reported %d deadlock signals", len(again))
	}
}
