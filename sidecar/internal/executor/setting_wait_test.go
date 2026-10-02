package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pg_reload_conf only signals the postmaster; every backend applies the
// new configuration when its own SIGHUP arrives. A post-check that reads
// a reloaded setting must therefore wait, bounded, for the value it
// expects, and still fail honestly when it never appears.
// No concurrent-access tests: awaitSetting holds no shared state; the
// reader it polls is the caller's.

// scriptedSetting returns old for the first n reads, then want.
func scriptedSetting(old, want int64, n int32) (func(context.Context) (int64, error),
	*atomic.Int32) {
	calls := &atomic.Int32{}
	return func(context.Context) (int64, error) {
		if calls.Add(1) <= n {
			return old, nil
		}
		return want, nil
	}, calls
}

func TestAwaitSetting_WaitsForTheReloadedValue(t *testing.T) {
	read, calls := scriptedSetting(-1, 512<<20, 2)
	got, err := awaitSetting(context.Background(), read, 512<<20, time.Second,
		time.Millisecond)
	if err != nil || got != 512<<20 || calls.Load() != 3 {
		t.Fatalf("got %d err %v after %d reads, want 536870912 after 3", got, err,
			calls.Load())
	}
}

func TestAwaitSetting_ImmediateMatchReadsOnce(t *testing.T) {
	read, calls := scriptedSetting(-1, 7, 0)
	start := time.Now()
	got, err := awaitSetting(context.Background(), read, 7, time.Second, time.Second)
	if err != nil || got != 7 || calls.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("got %d err %v reads %d in %v, want one immediate read", got, err,
			calls.Load(), time.Since(start))
	}
}

func TestAwaitSetting_NeverInEffectFailsWithTheLastValue(t *testing.T) {
	read, calls := scriptedSetting(-1, 9, 1<<30)
	start := time.Now()
	got, err := awaitSetting(context.Background(), read, 9, 60*time.Millisecond,
		5*time.Millisecond)
	if !errors.Is(err, errSettingNotInEffect) || got != -1 {
		t.Fatalf("got %d err %v, want the old value and errSettingNotInEffect", got, err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond || elapsed > time.Second {
		t.Fatalf("gave up after %v, want about the 60 ms bound", elapsed)
	}
	if calls.Load() < 2 {
		t.Fatalf("read %d times, want polling", calls.Load())
	}
}

// Boundary: a zero bound still reads once and judges that read.
func TestAwaitSetting_ZeroTimeoutReadsOnce(t *testing.T) {
	read, calls := scriptedSetting(1, 2, 1)
	if _, err := awaitSetting(context.Background(), read, 2, 0, time.Millisecond); !errors.Is(err,
		errSettingNotInEffect) || calls.Load() != 1 {
		t.Fatalf("err %v reads %d, want one read and errSettingNotInEffect", err, calls.Load())
	}
	read, calls = scriptedSetting(1, 2, 0)
	if got, err := awaitSetting(context.Background(), read, 2, 0, time.Millisecond); err != nil ||
		got != 2 || calls.Load() != 1 {
		t.Fatalf("got %d err %v reads %d, want an immediate match", got, err, calls.Load())
	}
}

func TestAwaitSetting_ReadErrorPropagates(t *testing.T) {
	boom := errors.New("connection refused")
	read := func(context.Context) (int64, error) { return 0, boom }
	_, err := awaitSetting(context.Background(), read, 1, time.Second, time.Millisecond)
	if !errors.Is(err, boom) || errors.Is(err, errSettingNotInEffect) ||
		!strings.Contains(err.Error(), "read setting") {
		t.Fatalf("err = %v, want the read error wrapped", err)
	}
}

func TestAwaitSetting_HonoursContext(t *testing.T) {
	read, _ := scriptedSetting(-1, 3, 1<<30)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := awaitSetting(ctx, read, 3, 10*time.Second, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v, want the context's deadline", err, time.Since(start))
	}
}
