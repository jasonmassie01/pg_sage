package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func quiesceExecutor() *Executor {
	return New(nil, config.DefaultConfig(), time.Time{}, nopLog)
}

func TestQuiesceIdleExecutorBlocksNewActionsUntilRelease(t *testing.T) {
	e := quiesceExecutor()
	release, err := e.Quiesce(context.Background())
	if err != nil || release == nil {
		t.Fatalf("idle quiesce = %v (release nil: %v)", err, release == nil)
	}
	if e.InFlightActions() != 0 {
		t.Fatalf("quiesce counted its own reservation as %d in-flight actions",
			e.InFlightActions())
	}
	if _, err := e.tryDDLSlot(); !errors.Is(err, ErrDDLSlotUnavailable) {
		t.Fatalf("new action during quiesce = %v, want ErrDDLSlotUnavailable", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.acquireDDLSlot(waitCtx); err == nil {
		t.Fatal("a waiting action acquired a slot while quiesced")
	}
	release()
	slot, err := e.tryDDLSlot()
	if err != nil {
		t.Fatalf("slot after release: %v", err)
	}
	slot()
	if len(e.ddlSem) != 0 {
		t.Fatalf("slots still held after release: %d", len(e.ddlSem))
	}
}

func TestQuiesceWaitsForInFlightActionToFinish(t *testing.T) {
	e := quiesceExecutor()
	finishAction, err := e.tryDDLSlot()
	if err != nil {
		t.Fatal(err)
	}
	if e.InFlightActions() != 1 {
		t.Fatalf("in-flight = %d, want 1", e.InFlightActions())
	}
	done := make(chan error, 1)
	var releaseQuiesce func()
	go func() {
		release, qerr := e.Quiesce(context.Background())
		releaseQuiesce = release
		done <- qerr
	}()
	select {
	case qerr := <-done:
		t.Fatalf("quiesce returned %v before the in-flight action finished", qerr)
	case <-time.After(50 * time.Millisecond):
	}
	finishAction()
	select {
	case qerr := <-done:
		if qerr != nil {
			t.Fatalf("quiesce after the action finished = %v", qerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce did not observe the finished action")
	}
	releaseQuiesce()
	if len(e.ddlSem) != 0 {
		t.Fatalf("slots leaked: %d", len(e.ddlSem))
	}
}

func TestQuiesceTimeoutReportsStillRunningActions(t *testing.T) {
	e := quiesceExecutor()
	first, _ := e.tryDDLSlot()
	second, _ := e.tryDDLSlot()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	release, err := e.Quiesce(ctx)
	if !errors.Is(err, ErrQuiesceTimeout) {
		t.Fatalf("quiesce = %v, want ErrQuiesceTimeout", err)
	}
	if !strings.Contains(err.Error(), "2 in-flight") {
		t.Fatalf("timeout error %q does not count the running actions", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("quiesce overran its bound: %v", elapsed)
	}
	if release == nil {
		t.Fatal("a timed-out quiesce must still return its partial release")
	}
	if _, err := e.tryDDLSlot(); !errors.Is(err, ErrDDLSlotUnavailable) {
		t.Fatal("a timed-out quiesce stopped blocking new actions")
	}
	release()
	first()
	second()
	if len(e.ddlSem) != 0 {
		t.Fatalf("slots leaked after timeout: %d", len(e.ddlSem))
	}
}

func TestQuiesceReleaseIsIdempotent(t *testing.T) {
	e := quiesceExecutor()
	release, err := e.Quiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	other, err := e.tryDDLSlot()
	if err != nil {
		t.Fatalf("double release corrupted the semaphore: %v", err)
	}
	other()
	if len(e.ddlSem) != 0 {
		t.Fatalf("slots = %d after balanced use", len(e.ddlSem))
	}
}

func TestQuiesceNilExecutorAndSemaphore(t *testing.T) {
	var nilExec *Executor
	release, err := nilExec.Quiesce(context.Background())
	if err != nil || release == nil {
		t.Fatalf("nil executor quiesce = %v (release nil: %v)", err, release == nil)
	}
	release()
	if nilExec.InFlightActions() != 0 {
		t.Fatal("nil executor reported in-flight actions")
	}
	e := quiesceExecutor()
	e.ddlSem = nil
	release, err = e.Quiesce(context.Background())
	if err != nil || release == nil {
		t.Fatalf("unbounded executor quiesce = %v", err)
	}
	release()
}

func TestQuiesceAlreadyCancelledContextStillBlocksNewWork(t *testing.T) {
	e := quiesceExecutor()
	held, _ := e.tryDDLSlot()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := e.Quiesce(ctx)
	if !errors.Is(err, ErrQuiesceTimeout) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled quiesce = %v, want ErrQuiesceTimeout wrapping cancel", err)
	}
	if _, err := e.tryDDLSlot(); !errors.Is(err, ErrDDLSlotUnavailable) {
		t.Fatal("cancelled quiesce left a free slot for new work")
	}
	release()
	held()
	if len(e.ddlSem) != 0 {
		t.Fatalf("slots leaked: %d", len(e.ddlSem))
	}
}

func TestApplyDuringQuiesceParksWithoutExecuting(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	release, err := e.Quiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	probe := &applyProbe{}
	_, err = e.Apply(context.Background(), probe.intent())
	if !errors.Is(err, ErrDDLSlotUnavailable) {
		t.Fatalf("Apply during quiesce = %v, want a park on the DDL slot", err)
	}
	if probe.executed.Load() != 0 {
		t.Fatal("an action executed while the executor was quiesced")
	}
}

func TestConcurrentQuiesceSecondCallerTimesOut(t *testing.T) {
	e := quiesceExecutor()
	release, err := e.Quiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var secondErr atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		other, qerr := e.Quiesce(ctx)
		secondErr.Store(qerr)
		other()
	}()
	<-done
	if got, _ := secondErr.Load().(error); !errors.Is(got, ErrQuiesceTimeout) {
		t.Fatalf("second quiesce = %v, want ErrQuiesceTimeout", got)
	}
	release()
	if len(e.ddlSem) != 0 {
		t.Fatalf("slots leaked: %d", len(e.ddlSem))
	}
}
