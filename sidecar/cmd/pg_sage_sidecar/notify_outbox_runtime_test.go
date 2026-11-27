package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-06: the runtime dispatcher queues in the outbox only once the
// process has a lifetime to drain it in; before that it delivers at once.
// No concurrent access tests: startup wiring, one goroutine per pool.

func TestStartNotifyOutbox_NeedsTheProcessLifetime(t *testing.T) {
	saved := shutdownCtx
	t.Cleanup(func() { shutdownCtx = saved })
	shutdownCtx = nil
	d := notify.NewDefaultDispatcher(notify.NewMemoryStore(), notify.TargetPolicy{}, nil)
	startNotifyOutbox(d, notify.NewPoolStore(nil, nil))
	if d.AlertsFindings() {
		t.Fatal("outbox enabled without a process lifetime")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the drain loop returns at once
	shutdownCtx = ctx
	startNotifyOutbox(d, notify.NewPoolStore(nil, nil))
	if !d.AlertsFindings() {
		t.Fatal("outbox not enabled with a process lifetime")
	}
}

func TestNotifyAlertPolicy_NilAndInvalidConfigKeepDefaults(t *testing.T) {
	p := notifyAlertPolicy(nil)
	want, _ := notify.NewAlertPolicy(0, nil, "", "", "")
	if p.String() != want.String() {
		t.Fatalf("nil config policy = %s, want the defaults %s", p, want)
	}
	c := &config.Config{Alerting: config.AlertingConfig{CooldownMinutes: 45,
		QuietHoursStart: "bad", QuietHoursEnd: "06:00", Timezone: "UTC"}}
	got := notifyAlertPolicy(c).String()
	if got == want.String() {
		t.Fatalf("cooldown_minutes 45 ignored: %s", got)
	}
	wantCustom, _ := notify.NewAlertPolicy(45, nil, "", "", "UTC")
	if got != wantCustom.String() {
		t.Fatalf("policy = %s, want %s (invalid quiet hours disabled)", got, wantCustom)
	}
}
