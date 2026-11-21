package main

import (
	"runtime"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/api"
)

// mcp-01b: wait_for_decision listens to the process's live-event broker
// through brokerSignals. Every broker event wakes a waiter; the broker's
// stop closes the waiter's channel; unsubscribing leaks no goroutine.

func signalWithin(t *testing.T, ch <-chan struct{}, d time.Duration) bool {
	t.Helper()
	select {
	case _, open := <-ch:
		return open
	case <-time.After(d):
		t.Fatalf("no signal within %s", d)
	}
	return false
}

func TestBrokerSignalsWakeOnEveryEventAndCloseOnStop(t *testing.T) {
	broker := api.NewEventBroker()
	signals := brokerSignals{broker: broker}
	ch, cancel := signals.Subscribe()
	defer cancel()
	for _, typ := range []api.EventType{api.EventDecisions, api.EventActions,
		api.EventHeartbeat} {
		broker.Publish(api.Event{Type: typ})
		if !signalWithin(t, ch, time.Second) {
			t.Fatalf("%s: the channel closed instead of signalling", typ)
		}
	}
	broker.Stop()
	if signalWithin(t, ch, time.Second) {
		t.Fatal("the channel stayed open after the broker stopped")
	}
}

func TestBrokerSignalsNilBrokerIsClosed(t *testing.T) {
	ch, cancel := brokerSignals{}.Subscribe()
	defer cancel()
	if signalWithin(t, ch, time.Second) {
		t.Fatal("no broker must mean a closed stream")
	}
}

func TestBrokerSignalsUnsubscribeLeaksNothing(t *testing.T) {
	broker := api.NewEventBroker()
	signals := brokerSignals{broker: broker}
	baseline := runtime.NumGoroutine()
	cancels := make([]func(), 0, 50)
	for i := 0; i < 50; i++ {
		_, cancel := signals.Subscribe()
		cancels = append(cancels, cancel)
	}
	for _, cancel := range cancels {
		cancel()
		cancel() // idempotent
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline || broker.SubscriberCount() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d (baseline %d), subscribers %d",
				runtime.NumGoroutine(), baseline, broker.SubscriberCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
