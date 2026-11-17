package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// guard-02b: the provider-event sources (restart, failover, version and
// host changes) run whenever the change feed store exists, even with
// sre.change_events.feed_enabled off; with it on, the full poller carries
// them and no second poller runs.
func TestSRESignals_ProviderEventsWithoutFeedPolling(t *testing.T) {
	settings := config.DefaultConfig().SRE
	settings.SLO.Enabled = false
	settings.ChangeEvents.FeedEnabled = false
	off := signalsFixture(t, settings).sig
	if off.poller != nil || off.providerPoller == nil || off.feed == nil {
		t.Fatalf("feed off: poller=%v providerPoller=%v feed=%v", off.poller,
			off.providerPoller, off.feed)
	}
	var workers int
	off.start(func(func()) { workers++ }, context.Background())
	if workers != 1 {
		t.Fatalf("feed off started %d workers, want the provider-event poller", workers)
	}
	settings.ChangeEvents.FeedEnabled = true
	on := signalsFixture(t, settings).sig
	if on.poller == nil || on.providerPoller != nil {
		t.Fatalf("feed on: poller=%v providerPoller=%v", on.poller, on.providerPoller)
	}
	workers = 0
	on.start(func(func()) { workers++ }, context.Background())
	if workers != 1 {
		t.Fatalf("feed on started %d workers, want the one full poller", workers)
	}
}
