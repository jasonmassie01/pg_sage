package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWave2ShutdownRejectsLateRollbackMonitor(t *testing.T) {
	e := New(nil, nil, nil, time.Time{}, func(string, string, ...any) {})
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	var ran atomic.Bool
	if e.startRollbackMonitor(func() { ran.Store(true) }) {
		t.Fatal("rollback monitor registered after executor shutdown")
	}
	if ran.Load() {
		t.Fatal("rejected rollback monitor still ran")
	}
}

func TestWave2ShutdownWaitsForRegisteredRollbackMonitor(t *testing.T) {
	e := New(nil, nil, nil, time.Time{}, func(string, string, ...any) {})
	started := make(chan struct{})
	if !e.startRollbackMonitor(func() {
		close(started)
		<-e.shutdownCh
	}) {
		t.Fatal("running executor rejected rollback monitor")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("rollback monitor did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestWave2RollbackMonitorRegistrationRacesShutdownSafely(t *testing.T) {
	e := New(nil, nil, nil, time.Time{}, func(string, string, ...any) {})
	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 64 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			e.startRollbackMonitor(func() { <-e.shutdownCh })
		}()
	}
	shutdownDone := make(chan error, 1)
	go func() {
		<-start
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownDone <- e.Shutdown(ctx)
	}()

	close(start)
	callers.Wait()
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if e.startRollbackMonitor(func() {}) {
		t.Fatal("executor accepted a monitor after concurrent shutdown")
	}
}
