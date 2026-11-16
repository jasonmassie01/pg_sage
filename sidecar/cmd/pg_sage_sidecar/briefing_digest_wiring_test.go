package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// ux-02b: briefing.slack_webhook_url and briefing.channels are reconciled
// into the managed Notifications channel config:slack:briefing at boot (in
// every alerting engine) and on a live briefing change, so the digest goes
// through Notifications.
// No concurrent access tests: the owners' locks are covered by the
// alerting and briefing owner tests.

const digestHook = "https://hooks.slack.example.com/services/T0/B9/briefing"

func briefingWithSlack() config.BriefingConfig {
	return config.BriefingConfig{Schedule: "0 6 * * *", Channels: []string{"stdout", "slack"},
		SlackWebhookURL: digestHook, OnlyWhenChanged: true}
}

func digestRuleChannels(t *testing.T, store *notify.MemoryStore) []string {
	t.Helper()
	rules, err := store.MatchingRules(context.Background(), notify.EventDigestReady)
	if err != nil {
		t.Fatalf("MatchingRules: %v", err)
	}
	var out []string
	for _, r := range rules {
		ch, err := store.Channel(context.Background(), r.ChannelID)
		if err != nil {
			t.Fatalf("Channel %d: %v", r.ChannelID, err)
		}
		out = append(out, ch.Name+"|"+r.MinSeverity)
	}
	return out
}

// State transition: the legacy engine reconciles no alerting rows, but the
// briefing channel is reconciled all the same.
func TestDigestBoot_ReconcilesTheBriefingChannelInEveryEngine(t *testing.T) {
	for _, engine := range []string{"", config.AlertingEngineLegacy} {
		withAlertingConfig(t, engineAlerting(engine, ""))
		cfg.Briefing = briefingWithSlack()
		store := notify.NewMemoryStore()
		res := newAlertingOwner().reconcileAtBoot(context.Background(), store)
		if res == nil || res.ChannelsCreated != 1 || res.RulesCreated != 1 {
			t.Fatalf("engine %q: boot reconcile = %+v, want the briefing channel and rule",
				engine, res)
		}
		got := strings.Join(digestRuleChannels(t, store), ",")
		if got != notify.BriefingChannelName+"|info" {
			t.Fatalf("engine %q: digest rules = %s, want one info rule on %s", engine, got,
				notify.BriefingChannelName)
		}
	}
}

// State transitions: a live briefing change adds the channel, its
// rollback removes it, and a legacy engine does not refuse it.
func TestBriefingOwner_LiveChangeReconcilesTheBriefingChannel(t *testing.T) {
	withAlertingConfig(t, engineAlerting(config.AlertingEngineLegacy, ""))
	store := notify.NewMemoryStore()
	alertingRuntime.addReconciler(store)
	t.Cleanup(func() {
		alertingRuntime.mu.Lock()
		delete(alertingRuntime.reconcilers, store)
		alertingRuntime.mu.Unlock()
	})
	before := &config.Config{Alerting: cfg.Alerting,
		Briefing: config.BriefingConfig{Schedule: "0 6 * * *", Channels: []string{"stdout"}}}
	after := &config.Config{Alerting: cfg.Alerting, Briefing: briefingWithSlack()}
	ctx := context.Background()
	p, err := newBriefingOwner().Prepare(ctx, config.ConfigSnapshot{Config: before},
		config.ConfigSnapshot{Config: after})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := digestRuleChannels(t, store); len(got) != 1 {
		t.Fatalf("digest rules after the change = %v, want the briefing channel's", got)
	}
	if err := p.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := digestRuleChannels(t, store); len(got) != 0 {
		t.Fatalf("digest rules after rollback = %v, want none", got)
	}
}

// Error propagation: a store that fails refuses the briefing change and
// names the channel.
func TestBriefingOwner_StoreErrorRefusesTheChange(t *testing.T) {
	withAlertingConfig(t, engineAlerting("", ""))
	store := notify.NewMemoryStore()
	store.FailReconcile, store.FailReconcileAt = context.DeadlineExceeded,
		notify.BriefingChannelName
	alertingRuntime.addReconciler(store)
	t.Cleanup(func() {
		alertingRuntime.mu.Lock()
		delete(alertingRuntime.reconcilers, store)
		alertingRuntime.mu.Unlock()
	})
	before := &config.Config{Alerting: cfg.Alerting}
	after := &config.Config{Alerting: cfg.Alerting, Briefing: briefingWithSlack()}
	_, err := newBriefingOwner().Prepare(context.Background(),
		config.ConfigSnapshot{Config: before}, config.ConfigSnapshot{Config: after})
	if err == nil || !strings.Contains(err.Error(), notify.BriefingChannelName) {
		t.Fatalf("Prepare err = %v, want a refusal naming %s", err, notify.BriefingChannelName)
	}
}

// Boundary: an unchanged briefing target reconciles nothing.
func TestBriefingTargetsChanged(t *testing.T) {
	a := briefingWithSlack()
	b := a
	b.Schedule = "0 7 * * *"
	if briefingTargetsChanged(a, b) {
		t.Fatal("a schedule change counted as a target change")
	}
	b.Channels = []string{"stdout"}
	if !briefingTargetsChanged(a, b) {
		t.Fatal("dropping slack from briefing.channels was not a target change")
	}
}

// Nil/empty: a runtime without a dispatcher has no notifier (the digest
// then goes to stdout and its row says so).
func TestBriefingNotifier_NilWithoutDispatcher(t *testing.T) {
	if n := (&databaseRuntime{}).briefingNotifier(); n != nil {
		t.Fatalf("notifier = %#v, want nil", n)
	}
}

// Error propagation: an inbox no source can read is an error, never a
// silent "nothing needs you".
func TestDigestInboxCounts_UnreadableIsNotZero(t *testing.T) {
	prev := fleetMgr
	fleetMgr = nil
	t.Cleanup(func() { fleetMgr = prev })
	counts, err := digestInboxCounts(context.Background(), "nowhere")
	if err == nil && !counts.LowerBound {
		t.Fatalf("counts = %+v with no error, want an error or a lower bound", counts)
	}
}
