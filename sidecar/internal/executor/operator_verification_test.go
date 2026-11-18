package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// plan-02d: a plan's operator steps are judged with this executor's v2
// readers after its verification window. No concurrent access test: the
// method reads the executor's configuration once and builds new values.

func TestOperatorVerification_WindowAndReaders(t *testing.T) {
	e := &Executor{logFn: func(string, string, ...any) {}}
	v2, window := e.OperatorVerification()
	if window != time.Duration(config.DefaultVerifyWindowMinutes)*time.Minute {
		t.Fatalf("no config: window %s, want the %d-minute default", window,
			config.DefaultVerifyWindowMinutes)
	}
	if v2.Counters == nil || v2.Mechanisms == nil || v2.Method == nil || v2.Cycle != time.Minute {
		t.Fatalf("no config: v2 %+v, want counters, mechanisms, a method and a 1m cycle", v2)
	}
	cfg := &config.Config{}
	cfg.Verify.WindowMinutes = 7
	cfg.Verify.Method = verify.MethodV1
	cfg.Collector.IntervalSeconds = 30
	e.cfg = cfg
	v2, window = e.OperatorVerification()
	if window != 7*time.Minute || v2.Cycle != 30*time.Second {
		t.Fatalf("configured: window %s cycle %s, want 7m and 30s", window, v2.Cycle)
	}
	if got := v2.Method(context.Background()); got != verify.MethodV1 {
		t.Fatalf("method %q, want the configured v1", got)
	}
	cfg.Verify.WindowMinutes = 0
	if _, window = e.OperatorVerification(); window != time.Duration(
		config.DefaultVerifyWindowMinutes)*time.Minute {
		t.Fatalf("zero window: %s, want the default", window)
	}
}
