package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/notify"
)

// facts-08: the facts worker tells the runtime about every confirmed fact
// that expires; the runtime sends one fact_expired notification for it.

type expiryRules struct {
	mu     sync.Mutex
	logged []notify.Event
}

func (r *expiryRules) MatchingRules(_ context.Context, event string) ([]notify.Rule,
	error) {
	if event != "fact_expired" {
		return nil, nil
	}
	return []notify.Rule{{ID: 1, ChannelID: 2, Event: event, MinSeverity: "warning",
		Enabled: true}}, nil
}

func (r *expiryRules) Channel(context.Context, int) (*notify.Channel, error) {
	return &notify.Channel{ID: 2, Type: "email", Enabled: true,
		Config: map[string]string{}}, nil
}

func (r *expiryRules) LogDelivery(_ context.Context, _ int, evt notify.Event,
	_, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logged = append(r.logged, evt)
	return nil
}

type expirySender struct{ sent []notify.Event }

func (s *expirySender) Type() string { return "email" }
func (s *expirySender) Send(_ context.Context, _ notify.Channel, evt notify.Event) error {
	s.sent = append(s.sent, evt)
	return nil
}

func expiredSlotFact(confirmed bool) facts.Fact {
	since := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	f := facts.Fact{ID: 12, Type: facts.TypeSlotConsumer, Kind: facts.KindSlot,
		Subject: "cdc_orders", Value: map[string]string{"consumer": "debezium"},
		Source: facts.SourceOperator, Status: facts.StatusExpired, MissingSince: &since,
		ExpiredReason: "no replication slot matches cdc_orders any more"}
	if confirmed {
		decided := since.Add(-24 * time.Hour)
		f.DecidedBy, f.DecidedAt = "alice@example.com", &decided
	}
	return f
}

func TestNotifyFactExpiredDispatchesOnlyLostConfirmations(t *testing.T) {
	rules := &expiryRules{}
	sender := &expirySender{}
	d := notify.NewDispatcherWithStore(rules, func(string, string, ...any) {})
	d.RegisterSender(sender)
	cfg := config.DefaultConfig()
	cfg.Facts.ConfirmedAbsenceDays = 30
	rt := &databaseRuntime{cfg: cfg, dispatcher: d,
		spec: databaseRuntimeSpec{Name: "orders"}}
	ctx := context.Background()
	rt.notifyFactExpired(ctx, expiredSlotFact(true))
	if len(sender.sent) != 1 || len(rules.logged) != 1 {
		t.Fatalf("a lost confirmation sent %d events (%d rows), want 1", len(sender.sent),
			len(rules.logged))
	}
	evt := sender.sent[0]
	if evt.Type != "fact_expired" || evt.Data["database"] != "orders" ||
		evt.Data["fact_id"] != int64(12) || !strings.Contains(evt.Body, "alice@example.com") ||
		!strings.Contains(evt.Body, "30 days") || !strings.Contains(evt.Subject, "debezium") {
		t.Fatalf("event %+v", evt)
	}
	rt.notifyFactExpired(ctx, expiredSlotFact(false))
	if len(sender.sent) != 1 {
		t.Fatalf("a proposal's expiry was notified: %d events", len(sender.sent))
	}
	(&databaseRuntime{spec: databaseRuntimeSpec{Name: "orders"}}).notifyFactExpired(ctx,
		expiredSlotFact(true)) // no dispatcher: nothing to send, no panic
}

// Error propagation: a failing rule store is logged, not fatal.
func TestNotifyFactExpiredSurvivesADispatchFailure(t *testing.T) {
	d := notify.NewDispatcherWithStore(&failingRules{}, func(string, string, ...any) {})
	rt := &databaseRuntime{cfg: config.DefaultConfig(), dispatcher: d,
		spec: databaseRuntimeSpec{Name: "orders"}}
	out := captureStderr(t, func() {
		rt.notifyFactExpired(context.Background(), expiredSlotFact(true))
	})
	if !strings.Contains(out, "fact 12 expiry not notified") ||
		!strings.Contains(out, "connection refused") {
		t.Fatalf("dispatch failure not logged with its cause: %q", out)
	}
}

type failingRules struct{ expiryRules }

func (*failingRules) MatchingRules(context.Context, string) ([]notify.Rule, error) {
	return nil, errors.New("control database: connection refused")
}
