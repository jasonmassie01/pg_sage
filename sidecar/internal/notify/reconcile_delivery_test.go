package notify

import (
	"context"
	"testing"
)

// alerting-06b happy path: one critical finding, routed by a reconciled
// YAML block to Slack and PagerDuty, is one Slack post and one PagerDuty
// trigger, although the analyzer emits finding_critical and
// finding_opened for it; its resolve closes the page under the same key.
func TestReconciledConfig_OneCriticalFindingIsOnePostPerChannel(t *testing.T) {
	slack, pd := &statusSeq{statuses: []int{200}}, &statusSeq{statuses: []int{202}}
	slackSrv, pdSrv := slack.server(t), pd.server(t)
	m := NewMemoryStore()
	a := baseAlerting()
	a.SlackWebhookURL, a.Webhooks = slackSrv.URL, nil
	a.Routes = a.Routes[:1]
	policy := TargetPolicy{AllowPrivate: true}
	if _, err := Reconcile(context.Background(), m, a, policy, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	d := NewDefaultDispatcher(m, policy, nil)
	d.RegisterSender(&PagerDutySender{client: pdSrv.Client(), apiURL: pdSrv.URL})
	d.UseOutbox(m)
	d.SetAlertPolicy(testPolicy(t, "", ""))
	ctx := context.Background()
	for _, evt := range []Event{
		FindingCriticalEvent("Bloat", "x", "db1", "bloat", "t"),
		FindingOpenedEvent("Bloat", "x", "db1", "bloat", "t", "critical"),
	} {
		if _, err := d.Submit(ctx, evt); err != nil {
			t.Fatalf("Submit %s: %v", evt.Type, err)
		}
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if hits, _ := slack.counts(); hits != 1 {
		t.Fatalf("slack posts = %d, want exactly 1", hits)
	}
	if hits, _ := pd.counts(); hits != 1 {
		t.Fatalf("pagerduty events = %d, want exactly 1 trigger", hits)
	}
	resolved := FindingResolvedEvent("Bloat", "db1", "bloat", "t", "critical")
	if _, err := d.Submit(ctx, resolved); err != nil {
		t.Fatalf("Submit resolve: %v", err)
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatalf("DrainOutbox resolve: %v", err)
	}
	pd.mu.Lock()
	bodies := append([]map[string]any(nil), pd.bodies...)
	pd.mu.Unlock()
	if len(bodies) != 2 || bodies[0]["event_action"] != "trigger" ||
		bodies[1]["event_action"] != "resolve" ||
		bodies[0]["dedup_key"] != bodies[1]["dedup_key"] {
		t.Fatalf("pagerduty bodies = %v, want a trigger and its resolve on one key", bodies)
	}
}
