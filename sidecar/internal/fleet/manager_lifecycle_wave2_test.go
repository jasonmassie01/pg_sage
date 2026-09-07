package fleet

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

func wave2LifecycleInstance(name string, databaseID int) *DatabaseInstance {
	return &DatabaseInstance{
		Name:       name,
		DatabaseID: databaseID,
		Config: config.DatabaseConfig{
			Name:     name,
			Host:     name + ".internal",
			Port:     5432,
			Database: name,
		},
		Pool:      &pgxpool.Pool{},
		PoolClose: func() {},
		Status:    &InstanceStatus{Connected: true, DatabaseName: name},
	}
}

func waitWave2Signal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestWave2RemoveDetachesBeforeCancelDrainAndClose(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(wave2LifecycleInstance("unrelated", 2))

	var eventMu sync.Mutex
	events := make([]string, 0, 4)
	record := func(event string) {
		eventMu.Lock()
		events = append(events, event)
		eventMu.Unlock()
	}
	cancelled := make(chan struct{})
	executorStopped := make(chan struct{})
	poolCloseStarted := make(chan struct{})
	releasePoolClose := make(chan struct{})
	workers := &sync.WaitGroup{}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-cancelled
		record("worker_drained")
	}()

	old := wave2LifecycleInstance("remove-me", 1)
	old.Workers = workers
	old.Cancel = func() {
		record("cancel")
		close(cancelled)
	}
	old.ExecutorShutdown = func(context.Context) error {
		record("executor_shutdown")
		close(executorStopped)
		return nil
	}
	old.PoolClose = func() {
		record("pool_close")
		close(poolCloseStarted)
		<-releasePoolClose
	}
	mgr.RegisterInstance(old)

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- mgr.RemoveInstanceContext(context.Background(), "remove-me")
	}()
	waitWave2Signal(t, poolCloseStarted, "pool close to begin")

	// Pool close is deliberately blocked. The removed instance must already
	// be absent and the manager lock must be available to unrelated traffic.
	lookupDone := make(chan *DatabaseInstance, 1)
	go func() { lookupDone <- mgr.GetInstance("remove-me") }()
	select {
	case got := <-lookupDone:
		if got != nil {
			t.Fatalf("removed instance remained visible during pool close: %#v", got)
		}
	case <-time.After(100 * time.Millisecond):
		close(releasePoolClose)
		t.Fatal("manager read blocked behind removed instance pool close")
	}
	if got := mgr.GetInstance("unrelated"); got == nil {
		close(releasePoolClose)
		t.Fatal("unrelated instance became unavailable during removal")
	}

	close(releasePoolClose)
	if err := <-removeDone; err != nil {
		t.Fatalf("remove: %v", err)
	}
	eventMu.Lock()
	gotEvents := append([]string(nil), events...)
	eventMu.Unlock()
	wantEvents := []string{
		"cancel", "worker_drained", "executor_shutdown", "pool_close",
	}
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("teardown events = %v, want %v", gotEvents, wantEvents)
	}
	for i := range wantEvents {
		if gotEvents[i] != wantEvents[i] {
			t.Fatalf("teardown events = %v, want %v", gotEvents, wantEvents)
		}
	}
}

func TestWave2RemoveInternalBoundClosesPoolWithStuckWorker(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	workers := &sync.WaitGroup{}
	workers.Add(1)
	poolClosed := make(chan struct{})
	inst := wave2LifecycleInstance("stuck", 1)
	inst.Workers = workers
	inst.teardownTimeout = 30 * time.Millisecond
	inst.Cancel = func() {}
	inst.ExecutorShutdown = func(context.Context) error { return nil }
	inst.PoolClose = func() { close(poolClosed) }
	mgr.RegisterInstance(inst)

	started := time.Now()
	err := mgr.RemoveInstanceContext(context.Background(), "stuck")
	if !errors.Is(err, context.DeadlineExceeded) {
		workers.Done()
		t.Fatalf("remove error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		workers.Done()
		t.Fatalf("context-bounded removal took %s", elapsed)
	}
	if mgr.GetInstance("stuck") != nil {
		workers.Done()
		t.Fatal("timed-out drain republished the detached instance")
	}
	waitWave2Signal(t, poolClosed, "pool close after internal worker deadline")

	// Release the waiter goroutine retained by sync.WaitGroup.Wait after the
	// lifecycle owner has already completed its bounded cleanup.
	workers.Done()
}

func TestWave2RemoveInternalBoundClosesPoolWithStuckExecutor(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	releaseExecutor := make(chan struct{})
	poolClosed := make(chan struct{})
	inst := wave2LifecycleInstance("stuck-executor", 1)
	inst.teardownTimeout = 30 * time.Millisecond
	inst.ExecutorShutdown = func(context.Context) error {
		<-releaseExecutor
		return nil
	}
	inst.PoolClose = func() { close(poolClosed) }
	mgr.RegisterInstance(inst)

	started := time.Now()
	err := mgr.RemoveInstanceContext(context.Background(), "stuck-executor")
	if !errors.Is(err, context.DeadlineExceeded) {
		close(releaseExecutor)
		t.Fatalf("remove error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		close(releaseExecutor)
		t.Fatalf("internally bounded executor shutdown took %s", elapsed)
	}
	waitWave2Signal(t, poolClosed, "pool close after executor deadline")
	close(releaseExecutor)
}

func TestWave2LifecycleMutationSerializesPublication(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	old := wave2LifecycleInstance("orders", 1)
	mgr.RegisterInstance(old)

	entered := make(chan struct{})
	release := make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- mgr.WithLifecycle(
			context.Background(),
			func(op *LifecycleMutation) error {
				if _, err := op.ValidateReplacement(
					"orders", "orders", old,
				); err != nil {
					return err
				}
				close(entered)
				<-release
				return nil
			},
		)
	}()
	waitWave2Signal(t, entered, "lifecycle owner")

	candidate := wave2LifecycleInstance("orders", 1)
	healthEntered := make(chan struct{})
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- mgr.ReplaceInstanceIfCurrent(
			context.Background(), "orders", old, candidate,
			func(context.Context, *DatabaseInstance) error {
				close(healthEntered)
				return nil
			},
		)
	}()
	select {
	case <-healthEntered:
		t.Fatal("replacement entered health check while lifecycle owner held reservation")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-ownerDone; err != nil {
		t.Fatalf("lifecycle owner: %v", err)
	}
	waitWave2Signal(t, healthEntered, "serialized replacement health check")
	if err := <-replaceDone; err != nil {
		t.Fatalf("replace: %v", err)
	}
}

func TestWave2ReconnectCASRejectsStaleCandidateAndRetiresIt(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	stale := wave2LifecycleInstance("orders", 1)
	mgr.RegisterInstance(stale)

	current := wave2LifecycleInstance("orders", 1)
	if err := mgr.ReplaceInstanceIfCurrent(
		context.Background(), "orders", stale, current,
		func(context.Context, *DatabaseInstance) error { return nil },
	); err != nil {
		t.Fatalf("publish current generation: %v", err)
	}

	reconnect := wave2LifecycleInstance("orders", 1)
	var reconnectClosed atomic.Int32
	reconnect.PoolClose = func() { reconnectClosed.Add(1) }
	healthCalled := atomic.Bool{}
	err := mgr.ReplaceInstanceIfCurrent(
		context.Background(), "orders", stale, reconnect,
		func(context.Context, *DatabaseInstance) error {
			healthCalled.Store(true)
			return nil
		},
	)
	if !errors.Is(err, ErrInstanceConflict) {
		t.Fatalf("stale reconnect error = %v, want ErrInstanceConflict", err)
	}
	if healthCalled.Load() {
		t.Fatal("stale reconnect candidate was health checked after identity rejection")
	}
	if got := mgr.GetInstance("orders"); got != current {
		t.Fatalf("active generation = %p, want current %p", got, current)
	}
	if reconnectClosed.Load() != 1 {
		t.Fatalf("rejected reconnect close calls = %d, want 1", reconnectClosed.Load())
	}
}

func TestWave2RemoveInvokesExecutorShutdownAndPoolCloseExactlyOnce(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := wave2LifecycleInstance("single-shot", 1)
	var shutdownCalls atomic.Int32
	var closeCalls atomic.Int32
	inst.ExecutorShutdown = func(context.Context) error {
		shutdownCalls.Add(1)
		return nil
	}
	inst.PoolClose = func() { closeCalls.Add(1) }
	mgr.RegisterInstance(inst)

	if err := mgr.RemoveInstanceContext(context.Background(), "single-shot"); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := mgr.RemoveInstanceContext(
		context.Background(), "single-shot",
	); !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("second remove error = %v, want ErrDatabaseNotFound", err)
	}
	if got := shutdownCalls.Load(); got != 1 {
		t.Fatalf("executor shutdown calls = %d, want 1", got)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("pool close calls = %d, want 1", got)
	}
}

func TestWave2ReplaceHealthCheckFailurePreservesOldAndRetiresCandidate(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	old := wave2LifecycleInstance("orders", 7)
	oldShutdown := atomic.Int32{}
	oldClose := atomic.Int32{}
	old.ExecutorShutdown = func(context.Context) error {
		oldShutdown.Add(1)
		return nil
	}
	old.PoolClose = func() { oldClose.Add(1) }
	mgr.RegisterInstance(old)

	candidate := wave2LifecycleInstance("orders-v2", 7)
	candidateShutdown := atomic.Int32{}
	candidateClose := atomic.Int32{}
	candidate.ExecutorShutdown = func(context.Context) error {
		candidateShutdown.Add(1)
		return nil
	}
	candidate.PoolClose = func() { candidateClose.Add(1) }
	healthErr := errors.New("candidate identity check failed")
	healthCalls := 0
	err := mgr.ReplaceInstance(
		context.Background(), "orders", candidate,
		func(_ context.Context, got *DatabaseInstance) error {
			healthCalls++
			if got != candidate {
				t.Fatalf("health check candidate = %p, want %p", got, candidate)
			}
			if mgr.GetInstance("orders") != old {
				t.Fatal("old generation was not active during candidate health check")
			}
			return healthErr
		},
	)
	if !errors.Is(err, healthErr) {
		t.Fatalf("replace error = %v, want %v", err, healthErr)
	}
	if healthCalls != 1 {
		t.Fatalf("health check calls = %d, want 1", healthCalls)
	}
	if got := mgr.GetInstance("orders"); got != old {
		t.Fatalf("active generation = %p, want preserved old %p", got, old)
	}
	if got := mgr.GetInstance("orders-v2"); got != nil {
		t.Fatalf("failed candidate was published: %#v", got)
	}
	if oldShutdown.Load() != 0 || oldClose.Load() != 0 {
		t.Fatal("old generation was torn down after candidate health failure")
	}
	if candidateShutdown.Load() != 1 || candidateClose.Load() != 1 {
		t.Fatalf("failed candidate teardown = shutdown:%d close:%d, want 1/1",
			candidateShutdown.Load(), candidateClose.Load())
	}
}

func TestWave2ReplaceHealthChecksThenAtomicallySwapsBeforeOldClose(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	old := wave2LifecycleInstance("orders", 7)
	oldPoolCloseStarted := make(chan struct{})
	releaseOldPoolClose := make(chan struct{})
	old.PoolClose = func() {
		close(oldPoolCloseStarted)
		<-releaseOldPoolClose
	}
	mgr.RegisterInstance(old)

	candidate := wave2LifecycleInstance("orders-v2", 7)
	candidate.Config.Host = "orders-v2.internal"
	candidate.Status.DatabaseName = "orders-v2"
	candidate.PoolClose = func() {
		t.Fatal("active candidate pool must not be closed")
	}
	healthStarted := make(chan struct{})
	releaseHealth := make(chan struct{})
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- mgr.ReplaceInstance(
			context.Background(), "orders", candidate,
			func(context.Context, *DatabaseInstance) error {
				close(healthStarted)
				<-releaseHealth
				return nil
			},
		)
	}()
	waitWave2Signal(t, healthStarted, "candidate health check")
	if got := mgr.GetInstance("orders"); got != old {
		t.Fatalf("old generation changed before health check passed: %p", got)
	}
	if got := mgr.GetInstance("orders-v2"); got != nil {
		t.Fatal("candidate was visible before its health check passed")
	}

	close(releaseHealth)
	waitWave2Signal(t, oldPoolCloseStarted, "old pool close")
	// Old close is blocked. Activation must already be atomic and manager
	// reads must not wait for retirement of the old generation.
	if got := mgr.GetInstance("orders"); got != nil {
		close(releaseOldPoolClose)
		t.Fatalf("old key survived atomic rename swap: %#v", got)
	}
	if got := mgr.GetInstance("orders-v2"); got != candidate {
		close(releaseOldPoolClose)
		t.Fatalf("active candidate = %p, want %p", got, candidate)
	}
	if got := mgr.GetInstanceByDatabaseID(7); got != candidate {
		close(releaseOldPoolClose)
		t.Fatalf("database ID lookup saw a gap or old generation: %p", got)
	}
	close(releaseOldPoolClose)
	if err := <-replaceDone; err != nil {
		t.Fatalf("replace: %v", err)
	}
	if candidate.Config.Host != "orders-v2.internal" {
		t.Fatalf("candidate host = %q, want orders-v2.internal",
			candidate.Config.Host)
	}
}

func TestWave2ReplaceRenameCollisionLeavesBothActiveGenerationsUntouched(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	old := wave2LifecycleInstance("orders", 1)
	existing := wave2LifecycleInstance("billing", 2)
	mgr.RegisterInstance(old)
	mgr.RegisterInstance(existing)

	candidate := wave2LifecycleInstance("billing", 1)
	var candidateClosed atomic.Int32
	candidate.PoolClose = func() { candidateClosed.Add(1) }
	err := mgr.ReplaceInstance(
		context.Background(), "orders", candidate,
		func(context.Context, *DatabaseInstance) error { return nil },
	)
	if err == nil {
		t.Fatal("expected rename collision error")
	}
	if mgr.GetInstance("orders") != old {
		t.Fatal("rename collision replaced the source instance")
	}
	if mgr.GetInstance("billing") != existing {
		t.Fatal("rename collision replaced the existing target instance")
	}
	if candidateClosed.Load() != 1 {
		t.Fatalf("rejected candidate pool close calls = %d, want 1",
			candidateClosed.Load())
	}
}

func TestWave2ReplaceContextCancellationBeforeHealthPassPreservesOld(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	old := wave2LifecycleInstance("orders", 1)
	mgr.RegisterInstance(old)
	candidate := wave2LifecycleInstance("orders", 1)
	var candidateClosed atomic.Int32
	candidate.PoolClose = func() { candidateClosed.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	healthCalled := atomic.Bool{}
	err := mgr.ReplaceInstance(
		ctx, "orders", candidate,
		func(context.Context, *DatabaseInstance) error {
			healthCalled.Store(true)
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replace error = %v, want context canceled", err)
	}
	if healthCalled.Load() {
		t.Fatal("health check ran after replacement context was canceled")
	}
	if mgr.GetInstance("orders") != old {
		t.Fatal("canceled replacement changed the active generation")
	}
	if candidateClosed.Load() != 1 {
		t.Fatalf("canceled candidate pool close calls = %d, want 1",
			candidateClosed.Load())
	}
}

// No concurrent access test is omitted: the blocking health-check and close
// tests above force reads to overlap both prepare and retire phases. Their
// assertions detect publication before validation, an activation gap, and
// manager-lock retention during teardown.
