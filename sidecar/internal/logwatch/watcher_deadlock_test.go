package logwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// rca-07b: the file watcher hands rca.deadlock_statement_text to the
// classifier it builds at Start.

// No concurrent access tests: the option is set before Start, which the
// watcher contract already requires to run before any Drain.

func TestFileWatcher_DeadlockStatementTextReachesClassifier(t *testing.T) {
	for _, mode := range []string{"", "fingerprint", "redacted"} {
		dir := t.TempDir() // the tailer starts on an existing log file
		if err := os.WriteFile(filepath.Join(dir, "postgresql.csv"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir,
			Format: "csvlog", PollIntervalMs: 50}, nopLog).WithDeadlockStatementText(mode)
		if err := fw.Start(context.Background()); err != nil {
			t.Fatalf("start: %v", err)
		}
		got := fw.classifier.redactedStatements
		fw.Stop()
		if got != (mode == "redacted") {
			t.Errorf("mode %q: classifier keeps text = %v", mode, got)
		}
	}
}
