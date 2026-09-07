package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func TestWave2DatabaseUpdateHookIsSynchronousAndPropagatesFailure(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	wantErr := errors.New("replacement health check failed")
	deps := &DatabaseDeps{
		OnUpdate: func(
			context.Context, store.DatabaseRecord, store.DatabaseRecord,
		) error {
			close(entered)
			<-release
			return wantErr
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- runDatabaseUpdateHook(
			context.Background(), deps,
			store.DatabaseRecord{Name: "old"},
			store.DatabaseRecord{Name: "new"},
		)
	}()
	waitDatabaseLifecycleSignal(t, entered, "update hook entry")
	select {
	case err := <-done:
		t.Fatalf("update returned before activation completed: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("update error = %v, want %v", err, wantErr)
	}
}

func TestWave2ApplyUpdateOwnsPersistenceBoundary(t *testing.T) {
	want := &store.DatabaseRecord{ID: 9, Name: "orders-v2"}
	called := false
	deps := &DatabaseDeps{
		ApplyUpdate: func(
			ctx context.Context,
			id int,
			old store.DatabaseRecord,
			input store.DatabaseInput,
		) (*store.DatabaseRecord, error) {
			called = true
			if ctx.Err() != nil || id != 9 || old.Name != "orders" {
				t.Fatalf("unexpected apply arguments: id=%d old=%q err=%v",
					id, old.Name, ctx.Err())
			}
			if input.Name != "orders-v2" {
				t.Fatalf("candidate name = %q, want orders-v2", input.Name)
			}
			return want, nil
		},
	}
	got, err := applyDatabaseUpdate(
		context.Background(), deps, 9,
		&store.DatabaseRecord{ID: 9, Name: "orders"},
		store.DatabaseInput{Name: "orders-v2"},
	)
	if err != nil {
		t.Fatalf("apply update: %v", err)
	}
	if !called || got != want {
		t.Fatalf("apply callback result = %p called=%t, want %p true",
			got, called, want)
	}
}

func TestWave2DeleteTeardownPropagatesRequestCancellation(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	workers := &sync.WaitGroup{}
	workers.Add(1)
	inst := &fleet.DatabaseInstance{
		Name:      "orders",
		Workers:   workers,
		Cancel:    func() {},
		PoolClose: func() {},
	}
	// Request cancellation is enough to prove the delete path does not hide
	// lifecycle failure; fleet tests separately cover the internal timeout.
	mgr.RegisterInstance(inst)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runDatabaseDeleteTeardown(ctx, &DatabaseDeps{Fleet: mgr}, "orders")
	workers.Done()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("delete teardown error = %v, want context canceled", err)
	}
}

func TestWave2DatabaseCreateHookUsesRequestContext(t *testing.T) {
	deps := &DatabaseDeps{
		OnCreate: func(ctx context.Context, _ store.DatabaseRecord) error {
			return ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDatabaseCreateHook(
		ctx, deps, store.DatabaseRecord{Name: "candidate"},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("create error = %v, want context canceled", err)
	}
}

func waitDatabaseLifecycleSignal(
	t *testing.T, signal <-chan struct{}, label string,
) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
