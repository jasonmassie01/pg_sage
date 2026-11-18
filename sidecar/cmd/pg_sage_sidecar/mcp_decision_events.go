package main

import (
	"sync"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// brokerSignals is the live-event broker as the stream MCP's
// wait_for_decision waits on (mcp-01b): every broker event (a successful
// API write, a change mark the poller saw, a heartbeat) wakes a waiter to
// re-read its item; the broker's stop closes the waiter's channel.
type brokerSignals struct{ broker *api.EventBroker }

var _ mcp.DecisionEvents = brokerSignals{}

// Subscribe relays the broker's events until cancel or the broker stops.
// One goroutine per subscription, ended by either.
func (s brokerSignals) Subscribe() (<-chan struct{}, func()) {
	out := make(chan struct{}, 1)
	if s.broker == nil {
		close(out)
		return out, func() {}
	}
	events, unsubscribe := s.broker.Subscribe()
	done := make(chan struct{})
	go relaySignals(events, out, done)
	var once sync.Once
	return out, func() {
		once.Do(func() {
			close(done)
			unsubscribe()
		})
	}
}

func relaySignals(events <-chan api.Event, out chan<- struct{}, done <-chan struct{}) {
	defer close(out)
	for {
		select {
		case <-done:
			return
		case _, open := <-events:
			if !open {
				return
			}
			select {
			case out <- struct{}{}:
			default: // a signal is already pending; the waiter re-reads once
			}
		}
	}
}
