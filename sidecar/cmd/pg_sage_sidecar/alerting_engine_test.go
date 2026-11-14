package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-06b: alerting.engine picks one notification engine. With the
// default notify engine no legacy manager starts and the alerting owner
// reconciles the YAML block into the notify tables, live; with legacy the
// old manager runs and nothing is reconciled.

func withAlertingConfig(t *testing.T, a config.AlertingConfig) {
	t.Helper()
	prev := cfg
	c := &config.Config{Alerting: a}
	c.NotificationPolicy.AllowPrivateTargets = true
	cfg = c
	t.Cleanup(func() { cfg = prev })
}

func engineAlerting(engine, slackURL string) config.AlertingConfig {
	return config.AlertingConfig{Enabled: true, Engine: engine, CooldownMinutes: 15,
		Timezone: "UTC", SlackWebhookURL: slackURL,
		Routes: []config.AlertRoute{{Severity: "critical", Channels: []string{"slack"}}}}
}

func unconnectedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// No connection is made until a query runs, and none runs here.
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestEngineSwitch_LegacyStartsAManagerNotifyDoesNot(t *testing.T) {
	pool := unconnectedPool(t)
	withAlertingConfig(t, engineAlerting(config.AlertingEngineLegacy,
		"https://hooks.slack.example.com/x"))
	if newInstanceAlertManager(pool, "db1") == nil {
		t.Fatal("engine legacy: want the legacy manager")
	}
	// The engine is restart-scoped: after the switch the next start of
	// the database runtime builds no legacy manager.
	withAlertingConfig(t, engineAlerting(config.AlertingEngineNotify,
		"https://hooks.slack.example.com/x"))
	if m := newInstanceAlertManager(pool, "db1"); m != nil {
		t.Fatal("engine notify: a legacy manager was built")
	}
	withAlertingConfig(t, engineAlerting("", "https://hooks.slack.example.com/x"))
	if m := newInstanceAlertManager(pool, "db1"); m != nil {
		t.Fatal("default engine: a legacy manager was built")
	}
}

func TestEngineNotify_OnePostPerFinding(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	withAlertingConfig(t, engineAlerting("", srv.URL))
	store := notify.NewMemoryStore()
	o := newAlertingOwner()
	if res := o.reconcileAtBoot(context.Background(), store); res == nil ||
		res.ChannelsCreated != 1 || res.RulesCreated != 2 {
		t.Fatalf("boot reconcile = %+v, want one channel and two rules", res)
	}
	if newInstanceAlertManager(unconnectedPool(t), "db1") != nil {
		t.Fatal("a legacy manager was built beside the notify engine")
	}
	d := notify.NewDefaultDispatcher(store, configTargetPolicy(cfg), nil)
	d.UseOutbox(store)
	d.SetAlertPolicy(notifyAlertPolicy(cfg))
	ctx := context.Background()
	for _, evt := range []notify.Event{
		notify.FindingCriticalEvent("Bloat", "x", "db1", "bloat", "t"),
		notify.FindingOpenedEvent("Bloat", "x", "db1", "bloat", "t", "critical"),
		notify.FindingCriticalEvent("Lock", "x", "db1", "locks", "u"),
		notify.FindingOpenedEvent("Lock", "x", "db1", "locks", "u", "critical"),
	} {
		if _, err := d.Submit(ctx, evt); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("slack posts = %d, want one per critical finding (2)", got)
	}
}

// The legacy manager delivers the block, so the switch back from notify
// removes the config rows a notify run left (else the outbox posts each
// finding a second time) and writes none; UI rows stay.
func TestEngineLegacy_BootRemovesConfigRowsOnly(t *testing.T) {
	ctx := context.Background()
	notifyBlock := engineAlerting("", "https://hooks.slack.example.com/x")
	withAlertingConfig(t, notifyBlock)
	store := notify.NewMemoryStore()
	uiID := store.AddChannel(notify.Channel{Name: "ops-ui", Type: "slack", Enabled: true,
		Config: map[string]string{"webhook_url": "https://hooks.slack.example.com/ui"}})
	store.AddRule(notify.Rule{ChannelID: uiID, Event: "finding_opened",
		MinSeverity: "warning", Enabled: true})
	if res := newAlertingOwner().reconcileAtBoot(ctx, store); res == nil ||
		res.ChannelsCreated != 1 {
		t.Fatalf("notify boot reconcile = %+v, want the config channel", res)
	}
	withAlertingConfig(t, engineAlerting(config.AlertingEngineLegacy,
		"https://hooks.slack.example.com/x"))
	res := newAlertingOwner().reconcileAtBoot(ctx, store)
	want := notify.ReconcileResult{ChannelsDeleted: 1, RulesDeleted: 2}
	if res == nil || *res != want {
		t.Fatalf("legacy boot reconcile = %+v, want %+v", res, want)
	}
	rules, err := store.MatchingRules(ctx, "finding_opened")
	if err != nil || len(rules) != 1 || rules[0].ChannelID != uiID {
		t.Fatalf("rules after the legacy boot = %+v, %v; want only the UI rule", rules, err)
	}
	if again := newAlertingOwner().reconcileAtBoot(ctx, store); again == nil ||
		*again != (notify.ReconcileResult{}) {
		t.Fatalf("second legacy boot = %+v, want nothing written", again)
	}
}

func TestAlertingOwner_ReconcilesALiveRouteChange(t *testing.T) {
	before := engineAlerting("", "https://hooks.slack.example.com/x")
	withAlertingConfig(t, before)
	store := notify.NewMemoryStore()
	o := newAlertingOwner()
	o.reconcileAtBoot(context.Background(), store)
	after := before
	after.Enabled = false
	ctx := context.Background()
	p, err := o.Prepare(ctx, alertingSnapshot(before), alertingSnapshot(after))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := store.Channel(ctx, 1); err == nil {
		t.Fatal("disabling alerting kept the config channel")
	}
	if err := p.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rules, _ := store.MatchingRules(ctx, "finding_opened"); len(rules) != 1 {
		t.Fatalf("rules after rollback = %v, want the route back", rules)
	}
}

func TestAlertingOwner_StoreErrorRefusesTheChange(t *testing.T) {
	before := engineAlerting("", "https://hooks.slack.example.com/x")
	withAlertingConfig(t, before)
	store := notify.NewMemoryStore()
	o := newAlertingOwner()
	o.reconcileAtBoot(context.Background(), store)
	after := before
	after.SlackWebhookURL = "https://hooks.slack.example.com/new"
	store.FailReconcile, store.FailReconcileAt = errors.New("disk full"),
		"config:slack:slack"
	_, err := o.Prepare(context.Background(), alertingSnapshot(before), alertingSnapshot(after))
	if err == nil || !strings.Contains(err.Error(), "config:slack:slack") {
		t.Fatalf("Prepare err = %v, want a refusal naming the channel", err)
	}
	store.FailReconcile = nil
	ch, err := store.Channel(context.Background(), 1)
	if err != nil || ch.Config["webhook_url"] != "https://hooks.slack.example.com/x" {
		t.Fatalf("channel after a refused change = %+v, %v; want the old URL", ch, err)
	}
}

func TestAlertingOwner_LegacyEngineRefusesALiveRouteChange(t *testing.T) {
	before := engineAlerting(config.AlertingEngineLegacy, "https://hooks.slack.example.com/x")
	after := before
	after.Routes = []config.AlertRoute{{Severity: "warning", Channels: []string{"slack"}}}
	_, err := newAlertingOwner().Prepare(context.Background(), alertingSnapshot(before),
		alertingSnapshot(after))
	if err == nil || !strings.Contains(err.Error(), "alerting.engine") ||
		!strings.Contains(err.Error(), "restart") {
		t.Fatalf("Prepare err = %v, want a refusal naming the engine and a restart", err)
	}
	policyOnly := before
	policyOnly.CooldownMinutes = 60
	if _, err := newAlertingOwner().Prepare(context.Background(), alertingSnapshot(before),
		alertingSnapshot(policyOnly)); err != nil {
		t.Fatalf("legacy cooldown change: %v, want it applied live as before", err)
	}
}
