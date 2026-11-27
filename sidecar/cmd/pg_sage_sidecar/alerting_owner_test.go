package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-06 CHECK-04: a live change of alerting.cooldown_minutes reaches
// the running notification dispatchers and the YAML managers' throttles,
// so the next deferral follows it without a restart.
// No concurrent access tests beyond the owner's mutex: Commit and add
// both take it; the dispatcher's policy swap is covered in notify.

func alertingSnapshot(a config.AlertingConfig) config.ConfigSnapshot {
	return config.ConfigSnapshot{Config: &config.Config{Alerting: a}}
}

func ownerDispatcher(t *testing.T, a config.AlertingConfig) (*notify.Dispatcher,
	*notify.MemoryStore) {
	t.Helper()
	store := notify.NewMemoryStore()
	id := store.AddChannel(notify.Channel{Name: "s", Type: "slack", Enabled: true,
		Config: map[string]string{"webhook_url": "https://hooks.example.com/x"}})
	for _, ev := range []string{"finding_opened", "finding_resolved"} {
		store.AddRule(notify.Rule{ChannelID: id, Event: ev, MinSeverity: "info",
			Enabled: true})
	}
	d := notify.NewDefaultDispatcher(store, notify.TargetPolicy{}, nil)
	d.UseOutbox(store)
	d.SetAlertPolicy(notifyAlertPolicy(&config.Config{Alerting: a}))
	return d, store
}

// reopenDelay resolves and reopens a finding and returns how far in the
// future the reopened alert is scheduled: the cooldown.
func reopenDelay(t *testing.T, d *notify.Dispatcher, store *notify.MemoryStore) time.Duration {
	t.Helper()
	ctx := context.Background()
	for _, evt := range []notify.Event{
		notify.FindingOpenedEvent("Bloat", "{}", "db1", "bloat", "t", "warning"),
		notify.FindingResolvedEvent("Bloat", "db1", "bloat", "t", "warning"),
		notify.FindingOpenedEvent("Bloat", "{}", "db1", "bloat", "t", "warning"),
	} {
		if _, err := d.Submit(ctx, evt); err != nil {
			t.Fatal(err)
		}
	}
	out := store.Outbox()
	return time.Until(out[len(out)-1].NextAttemptAt)
}

func TestAlertingOwner_LiveCooldownChangesTheNextDeferral(t *testing.T) {
	before := config.AlertingConfig{CooldownMinutes: 15, Timezone: "UTC"}
	d, store := ownerDispatcher(t, before)
	o := newAlertingOwner()
	o.addDispatcher(d)
	if got := reopenDelay(t, d, store); got < 25*time.Minute || got > 31*time.Minute {
		t.Fatalf("reopen delay before = %v, want the 30m warning floor", got)
	}
	after := before
	after.CooldownMinutes = 120
	p, err := o.Prepare(context.Background(), alertingSnapshot(before), alertingSnapshot(after))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := p.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// The second episode waits 120m from the reopened alert's own time.
	if got := reopenDelay(t, d, store); got < 145*time.Minute || got > 151*time.Minute {
		t.Fatalf("reopen delay after = %v, want 30m + 120m", got)
	}
}

func TestAlertingOwner_PrepareRefusesInvalidValues(t *testing.T) {
	o := newAlertingOwner()
	ok := config.AlertingConfig{CooldownMinutes: 15}
	for name, bad := range map[string]config.AlertingConfig{
		"timezone":    {Timezone: "Nowhere/City"},
		"quiet hours": {QuietHoursStart: "25:00", QuietHoursEnd: "06:00"},
	} {
		_, err := o.Prepare(context.Background(), alertingSnapshot(ok), alertingSnapshot(bad))
		if err == nil || !strings.Contains(err.Error(), "alerting.") {
			t.Errorf("%s: Prepare err = %v, want a refusal naming the field", name, err)
		}
	}
	if _, err := o.Prepare(context.Background(), alertingSnapshot(ok),
		config.ConfigSnapshot{}); err == nil {
		t.Error("Prepare accepted a nil desired config")
	}
}

func TestAlertingOwner_CommitAndRollbackReachThrottles(t *testing.T) {
	o := newAlertingOwner()
	th := alerting.NewThrottle(15, "", "", "UTC")
	o.addThrottle(th)
	night := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	before := config.AlertingConfig{CooldownMinutes: 15}
	after := config.AlertingConfig{CooldownMinutes: 15, QuietHoursStart: "22:00",
		QuietHoursEnd: "06:00"}
	p, err := o.Prepare(context.Background(), alertingSnapshot(before), alertingSnapshot(after))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !th.IsQuietHours(night) {
		t.Fatal("committed quiet hours did not reach the throttle")
	}
	if err := p.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if th.IsQuietHours(night) {
		t.Fatal("rollback left the new quiet hours on the throttle")
	}
	if o.Name() != config.AlertingOwner {
		t.Fatalf("Name = %q", o.Name())
	}
}

func TestOwnersForMode_RegistersAlertingEverywhere(t *testing.T) {
	for _, mode := range []string{"standalone", "fleet"} {
		c := modeConfig(mode)
		found := false
		for _, o := range ownersForMode(c) {
			found = found || o == config.AlertingOwner
		}
		if !found {
			t.Errorf("%s mode does not register the alerting owner", mode)
		}
	}
}
