package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

type teardownEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *teardownEvents) add(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *teardownEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func quiescedInstance(name string, events *teardownEvents) *DatabaseInstance {
	inst := wave2LifecycleInstance(name, 7)
	inst.Cancel = func() { events.add("cancel") }
	inst.ExecutorShutdown = func(context.Context) error {
		events.add("executor_shutdown")
		return nil
	}
	inst.PoolClose = func() { events.add("pool_close") }
	return inst
}

func TestTeardownQuiescesBeforeCancelAndReleasesAfter(t *testing.T) {
	events := &teardownEvents{}
	inst := quiescedInstance("drain-me", events)
	inst.Quiesce = func(ctx context.Context) (func(), error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("quiesce ran without a drain deadline")
		}
		events.add("quiesce")
		return func() { events.add("release") }, nil
	}
	if err := ShutdownInstance(context.Background(), inst); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	want := []string{"quiesce", "cancel", "release", "executor_shutdown", "pool_close"}
	if got := events.list(); !equalStrings(got, want) {
		t.Fatalf("teardown order = %v, want %v", got, want)
	}
}

func TestTeardownDrainTimeoutBoundsQuiesceAndStillCompletes(t *testing.T) {
	events := &teardownEvents{}
	inst := quiescedInstance("slow-action", events)
	inst.DrainTimeout = 50 * time.Millisecond
	sentinel := errors.New("1 in-flight action still running")
	inst.Quiesce = func(ctx context.Context) (func(), error) {
		<-ctx.Done()
		events.add("quiesce_timeout")
		return func() { events.add("release") }, errors.Join(sentinel, ctx.Err())
	}
	started := time.Now()
	err := ShutdownInstance(context.Background(), inst)
	if !errors.Is(err, sentinel) {
		t.Fatalf("shutdown error = %v, want the drain error surfaced", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("drain timeout did not bound teardown: %v", elapsed)
	}
	want := []string{"quiesce_timeout", "cancel", "release", "executor_shutdown", "pool_close"}
	if got := events.list(); !equalStrings(got, want) {
		t.Fatalf("teardown order = %v, want %v", got, want)
	}
}

func TestTeardownWithoutQuiesceKeepsLegacyOrder(t *testing.T) {
	events := &teardownEvents{}
	inst := quiescedInstance("legacy", events)
	if err := ShutdownInstance(context.Background(), inst); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	want := []string{"cancel", "executor_shutdown", "pool_close"}
	if got := events.list(); !equalStrings(got, want) {
		t.Fatalf("teardown order = %v, want %v", got, want)
	}
}

func TestTeardownQuiesceNilReleaseIsTolerated(t *testing.T) {
	events := &teardownEvents{}
	inst := quiescedInstance("nil-release", events)
	inst.Quiesce = func(context.Context) (func(), error) { return nil, nil }
	if err := ShutdownInstance(context.Background(), inst); err != nil {
		t.Fatalf("shutdown with nil release: %v", err)
	}
	if got := events.list(); len(got) != 3 || got[0] != "cancel" {
		t.Fatalf("teardown events = %v", got)
	}
}

func TestTeardownQuiesceRunsOnceForConcurrentRemovers(t *testing.T) {
	events := &teardownEvents{}
	inst := quiescedInstance("shared", events)
	inst.Quiesce = func(context.Context) (func(), error) {
		events.add("quiesce")
		return func() {}, nil
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ShutdownInstance(context.Background(), inst); err != nil {
				t.Errorf("concurrent shutdown: %v", err)
			}
		}()
	}
	wg.Wait()
	count := 0
	for _, event := range events.list() {
		if event == "quiesce" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("quiesce ran %d times, want once", count)
	}
}

func TestLifecycleUpdateMetadataSwapsConfigOfCurrentGeneration(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := wave2LifecycleInstance("orders", 3)
	mgr.RegisterInstance(inst)
	updated := inst.Config
	updated.Tags = []string{"payments"}
	updated.TrustLevel = "advisory"
	err := mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.UpdateMetadata(inst, updated)
	})
	if err != nil {
		t.Fatalf("update metadata: %v", err)
	}
	got := mgr.GetInstance("orders")
	if got != inst || !got.Config.HasTag("payments") || got.Config.TrustLevel != "advisory" {
		t.Fatalf("metadata not applied in place: %+v", got.Config)
	}
}

func TestLifecycleUpdateMetadataRejectsStaleRenamedAndNil(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	current := wave2LifecycleInstance("orders", 3)
	mgr.RegisterInstance(current)
	stale := wave2LifecycleInstance("orders", 3)
	renamed := current.Config
	renamed.Name = "billing"
	cases := []struct {
		name     string
		expected *DatabaseInstance
		cfg      config.DatabaseConfig
		want     error
	}{
		{"stale generation", stale, current.Config, ErrInstanceConflict},
		{"rename", current, renamed, ErrInvalidInstance},
		{"nil instance", nil, current.Config, ErrInvalidInstance},
	}
	for _, tc := range cases {
		err := mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
			return op.UpdateMetadata(tc.expected, tc.cfg)
		})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := mgr.GetInstance("orders"); got != current || got.Config.Name != "orders" {
		t.Fatal("a rejected metadata update changed the published instance")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
