package providerobs

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-09b: a configured provider log sink counts as a detected log
// source, so the default (logwatch.enabled unset: auto) classifies its
// records for RCA; explicit false still keeps them out.
// No concurrent access tests: TestLogSinkBoundsQueueAndConcurrentDrain
// covers the sink's locking; this only changes construction.
func TestLogSinkAutoModeClassifiesProviderLogs(t *testing.T) {
	deadlock := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
		ErrorLevel: "ERROR", SQLState: "40P01", Message: "deadlock detected"}
	off := enabledRef(false)
	for _, c := range []struct {
		name    string
		enabled *bool
		rca     bool
		want    int
	}{
		{"auto", nil, true, 1},
		{"explicit false", off, true, 0},
		{"auto without rca", nil, false, 0},
	} {
		cfg := config.DefaultConfig()
		cfg.LogWatch.Enabled, cfg.RCA.Enabled = c.enabled, c.rca
		sink, err := NewLogSink(nil, "postgres", cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := sink.Handle(context.Background(), []logwatch.LogEntry{deadlock}); err != nil {
			t.Fatalf("%s: handle: %v", c.name, err)
		}
		signals := sink.Drain()
		if len(signals) != c.want {
			t.Fatalf("%s: %d signals, want %d", c.name, len(signals), c.want)
		}
		if c.want == 1 && signals[0].ID != "log_deadlock_detected" {
			t.Fatalf("%s: signal %s, want log_deadlock_detected", c.name, signals[0].ID)
		}
	}
}

func enabledRef(on bool) *bool { return &on }
