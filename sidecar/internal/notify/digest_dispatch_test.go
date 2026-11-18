package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ux-02b: Submit says what happened on each channel, so the digest row can
// name the channel that failed; digest_ready is delivered at once even
// when the dispatcher uses the outbox.
// No concurrent access tests: Submit shares no state between calls beyond
// the store, whose concurrency the outbox tests cover.

type failingSender struct{ typ string }

func (f failingSender) Type() string { return f.typ }
func (f failingSender) Send(context.Context, Channel, Event) error {
	return errors.New("webhook answered HTTP 500")
}

func digestEvent() Event {
	return Event{Type: EventDigestReady, Severity: "info", Subject: "pg_sage digest",
		Body: "6 changes", Data: map[string]any{"database": "prod"}}
}

func TestSubmit_ReportsEachChannelsOutcome(t *testing.T) {
	m := NewMemoryStore()
	okID := m.AddChannel(Channel{Name: "team-slack", Type: "slack", Enabled: true})
	badID := m.AddChannel(Channel{Name: "ops-hook", Type: "webhook", Enabled: true})
	m.AddRule(Rule{ChannelID: okID, Event: EventDigestReady, MinSeverity: "info", Enabled: true})
	m.AddRule(Rule{ChannelID: badID, Event: EventDigestReady, MinSeverity: "info",
		Enabled: true})
	d := NewDispatcherWithStore(m, func(string, string, ...any) {})
	ok := newMockSender("slack")
	d.RegisterSender(ok)
	d.RegisterSender(failingSender{typ: "webhook"})
	d.UseOutbox(m)

	res, outs, err := d.SubmitWithOutcomes(context.Background(), digestEvent())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Delivered != 1 || res.Failed != 1 || res.Enqueued != 0 {
		t.Fatalf("result = %+v, want 1 delivered, 1 failed, none queued", res)
	}
	if len(m.Outbox()) != 0 {
		t.Fatalf("outbox = %v, want empty: digest_ready is delivered at once", m.Outbox())
	}
	got := map[string]ChannelOutcome{}
	for _, o := range outs {
		got[o.Channel] = o
	}
	if o := got["team-slack"]; o.Status != OutcomeSent || o.ChannelID != okID || o.Error != "" {
		t.Errorf("team-slack outcome = %+v, want sent on channel %d", o, okID)
	}
	if o := got["ops-hook"]; o.Status != OutcomeError || !strings.Contains(o.Error, "HTTP 500") {
		t.Errorf("ops-hook outcome = %+v, want error naming HTTP 500", o)
	}
	var logged []string
	for _, dl := range m.Deliveries() {
		logged = append(logged, dl.Status)
	}
	if strings.Join(logged, ",") != "sent,error" && strings.Join(logged, ",") != "error,sent" {
		t.Errorf("notification log statuses = %v, want one sent and one error", logged)
	}
}

// Error propagation: a channel without a sender is refused and named.
func TestSubmit_OutcomeNamesARefusedChannel(t *testing.T) {
	m := NewMemoryStore()
	id := m.AddChannel(Channel{Name: "carrier-pigeon", Type: "pigeon", Enabled: true})
	m.AddRule(Rule{ChannelID: id, Event: EventDigestReady, MinSeverity: "info", Enabled: true})
	d := NewDispatcherWithStore(m, func(string, string, ...any) {})
	_, outs, err := d.SubmitWithOutcomes(context.Background(), digestEvent())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(outs) != 1 || outs[0].Status != OutcomeRefused ||
		outs[0].ChannelID != id || !strings.Contains(outs[0].Error, "pigeon") {
		t.Fatalf("outcomes = %+v, want one refused outcome naming the missing sender",
			outs)
	}
}

// Nil/empty: no matching rule gives no outcomes and no error.
func TestSubmit_NoRuleGivesNoOutcomes(t *testing.T) {
	d := NewDispatcherWithStore(NewMemoryStore(), func(string, string, ...any) {})
	res, outs, err := d.SubmitWithOutcomes(context.Background(), digestEvent())
	if err != nil || len(outs) != 0 || res.Delivered != 0 {
		t.Fatalf("Submit = %+v, %v; want nothing and no error", res, err)
	}
}

// Other events still queue and are reported as queued.
func TestSubmit_QueuedEventReportsQueued(t *testing.T) {
	m := NewMemoryStore()
	id := m.AddChannel(Channel{Name: "team-slack", Type: "slack", Enabled: true})
	m.AddRule(Rule{ChannelID: id, Event: "action_executed", MinSeverity: "info",
		Enabled: true})
	d := NewDispatcherWithStore(m, func(string, string, ...any) {})
	d.RegisterSender(newMockSender("slack"))
	d.UseOutbox(m)
	res, outs, err := d.SubmitWithOutcomes(context.Background(), Event{Type: "action_executed",
		Severity: "info", Subject: "s"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Enqueued != 1 || len(outs) != 1 || outs[0].Status != OutcomeQueued {
		t.Fatalf("result = %+v, want one queued outcome", res)
	}
}
