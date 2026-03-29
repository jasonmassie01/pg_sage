package store

import (
	"context"
	"testing"
	"time"
)

func TestStartActionExpiry_ExitsOnContextCancel(t *testing.T) {
	// StartActionExpiry requires an ActionStore with a real pool
	// for the ExpireStale call. Since we only want to verify the
	// goroutine exits on context cancellation (not that it
	// successfully expires anything), we pass a nil-pool store.
	// The ticker fires every hour by default, so we cancel
	// immediately and verify the goroutine returns.

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var logCalls int
	logFn := func(component, msg string, args ...any) {
		logCalls++
	}

	// Cancel before starting so the goroutine sees ctx.Done()
	// on its first select.
	cancel()

	go func() {
		StartActionExpiry(ctx, &ActionStore{}, logFn)
		close(done)
	}()

	select {
	case <-done:
		// Goroutine exited as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("StartActionExpiry did not exit within 2s " +
			"after context cancellation")
	}
}

func TestStartActionExpiry_ExitsOnDelayedCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	logFn := func(component, msg string, args ...any) {}

	go func() {
		StartActionExpiry(ctx, &ActionStore{}, logFn)
		close(done)
	}()

	// Give the goroutine time to start and block on select.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Goroutine exited as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("StartActionExpiry did not exit within 2s " +
			"after delayed context cancellation")
	}
}
