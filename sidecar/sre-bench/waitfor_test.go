package srebench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitForWithin polls cond until it holds, it errors, the deadline passes
// or ctx ends; the timeout error names what was awaited and for how long.
func TestWaitForWithin(t *testing.T) {
	ctx := context.Background()
	calls := 0
	if err := waitForWithin(ctx, "ready", time.Second, func() (bool, error) {
		calls++
		return calls == 3, nil
	}); err != nil || calls != 3 {
		t.Fatalf("waitForWithin = %v after %d calls, want nil after 3", err, calls)
	}
	start := time.Now()
	err := waitForWithin(ctx, "slot", 120*time.Millisecond, func() (bool, error) {
		return false, nil
	})
	if err == nil || err.Error() != "slot: not reached in 0.12 s" {
		t.Fatalf("timeout error = %v", err)
	}
	if waited := time.Since(start); waited < 120*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %v for a 120 ms deadline", waited)
	}
	boom := errors.New("boom")
	if err := waitForWithin(ctx, "probe", time.Second, func() (bool, error) {
		return false, boom
	}); !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "probe: ") {
		t.Fatalf("condition error = %v, want it wrapped with what was awaited", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := waitForWithin(cctx, "x", time.Minute, func() (bool, error) {
		return false, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}
}

// waitFor keeps its 15 s deadline and message.
func TestWaitForDefaultDeadline(t *testing.T) {
	if waitDeadline != 15*time.Second {
		t.Fatalf("waitDeadline = %v", waitDeadline)
	}
}

// A logical slot's restart_lsn (and so the WAL it retains) moves only at a
// running-xacts record, which the background writer logs at most every
// 15 s. Waiting for a keeping-up slot must outlast two of those intervals,
// or the decoy's setup races the server (v2.2.0 tag build: "slot consumer
// to confirm: not reached in 15 s").
func TestSlotConfirmWaitOutlastsRunningXactsRecords(t *testing.T) {
	if slotConfirmWait < 2*runningXactsInterval+5*time.Second {
		t.Fatalf("slotConfirmWait = %v, want at least two %v intervals plus slack",
			slotConfirmWait, runningXactsInterval)
	}
}
