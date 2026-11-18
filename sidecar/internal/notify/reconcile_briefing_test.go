package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// ux-02b: briefing.channels and briefing.slack_webhook_url are reconciled
// into one config-managed channel, config:slack:briefing, with a
// digest_ready rule, so the digest goes through Notifications and the
// briefing has no Slack post path of its own.
// No concurrent access tests: the reconcile transaction's lock is covered
// by reconcile_db_test.go.

const testBriefingSlack = "https://hooks.slack.example.com/services/T0/B9/briefing"

func briefingBlock(url string, channels ...string) config.BriefingConfig {
	return config.BriefingConfig{Schedule: "0 6 * * *", Channels: channels,
		SlackWebhookURL: url}
}

func reconcileWithBriefing(
	t *testing.T, m *MemoryStore, a config.AlertingConfig, b config.BriefingConfig,
) (ReconcileResult, *reconcileLog) {
	t.Helper()
	logs := &reconcileLog{}
	res, err := Reconcile(context.Background(), m, a, TargetPolicy{}, logs.fn,
		WithBriefing(b))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res, logs
}

// State transitions: set -> managed channel and digest rule; unchanged ->
// no writes; removed from YAML -> channel and rule gone.
func TestReconcileBriefing_SetThenUnchangedThenRemoved(t *testing.T) {
	m := NewMemoryStore()
	b := briefingBlock(testBriefingSlack, "stdout", "slack")
	res, _ := reconcileWithBriefing(t, m, config.AlertingConfig{}, b)
	if res != (ReconcileResult{ChannelsCreated: 1, RulesCreated: 1}) {
		t.Fatalf("first reconcile = %+v, want 1 channel and 1 rule", res)
	}
	if got := strings.Join(channelNames(m), ","); got != "config:slack:briefing|slack|config" {
		t.Fatalf("channels = %s, want the managed briefing channel", got)
	}
	if got := strings.Join(ruleView(m), ","); got !=
		"config:slack:briefing|digest_ready|info|config" {
		t.Fatalf("rules = %s, want one digest_ready info rule", got)
	}
	m.mu.Lock()
	var cfgURL string
	for _, ch := range m.channels {
		cfgURL = ch.Config["webhook_url"]
	}
	m.mu.Unlock()
	if cfgURL != testBriefingSlack {
		t.Fatalf("channel webhook_url = %q, want the briefing webhook", cfgURL)
	}
	again, _ := reconcileWithBriefing(t, m, config.AlertingConfig{}, b)
	if again != (ReconcileResult{}) {
		t.Fatalf("second reconcile = %+v, want no writes", again)
	}
	gone, _ := reconcileWithBriefing(t, m, config.AlertingConfig{},
		briefingBlock("", "stdout"))
	if gone != (ReconcileResult{ChannelsDeleted: 1, RulesDeleted: 1}) {
		t.Fatalf("removal reconcile = %+v, want the channel and its rule deleted", gone)
	}
	if len(channelNames(m)) != 0 || len(ruleView(m)) != 0 {
		t.Fatalf("left behind: channels %v rules %v", channelNames(m), ruleView(m))
	}
}

// The briefing channel lives beside the alerting block's channels; each
// keeps its own rules.
func TestReconcileBriefing_KeepsAlertingChannels(t *testing.T) {
	m := NewMemoryStore()
	res, _ := reconcileWithBriefing(t, m, baseAlerting(),
		briefingBlock(testBriefingSlack, "slack"))
	if res != (ReconcileResult{ChannelsCreated: 4, RulesCreated: 7}) {
		t.Fatalf("reconcile = %+v, want 3 alerting + 1 briefing channel, 6 + 1 rules", res)
	}
	view := strings.Join(ruleView(m), ",")
	if !strings.Contains(view, "config:slack:briefing|digest_ready|info|config") ||
		!strings.Contains(view, "config:slack:slack|finding_opened|critical|config") {
		t.Fatalf("rules = %s, want both the digest and the alerting rules", view)
	}
	// Without the option (the old call) the briefing channel is not wanted.
	logs := &reconcileLog{}
	res, err := Reconcile(context.Background(), m, baseAlerting(), TargetPolicy{}, logs.fn)
	if err != nil || res != (ReconcileResult{ChannelsDeleted: 1, RulesDeleted: 1}) {
		t.Fatalf("reconcile without briefing = %+v, %v; want only it removed", res, err)
	}
}

// Nil/empty: a webhook without "slack" in briefing.channels, or "slack"
// without a webhook, builds nothing.
func TestReconcileBriefing_NeedsBothTheChannelAndTheURL(t *testing.T) {
	for name, b := range map[string]config.BriefingConfig{
		"url only":   briefingBlock(testBriefingSlack, "stdout"),
		"slack only": briefingBlock("", "slack"),
		"empty":      {},
	} {
		m := NewMemoryStore()
		if res, _ := reconcileWithBriefing(t, m, config.AlertingConfig{}, b); res !=
			(ReconcileResult{}) {
			t.Errorf("%s: reconcile = %+v, want nothing", name, res)
		}
	}
}

// Invalid input: a webhook the target policy refuses builds no channel and
// the log names the key, never the URL's secret path.
func TestReconcileBriefing_RefusedURLIsLoggedWithoutSecret(t *testing.T) {
	m := NewMemoryStore()
	logs := &reconcileLog{}
	b := briefingBlock("http://169.254.169.254/services/T0/B9/SECRETPATH", "slack")
	res, err := Reconcile(context.Background(), m, config.AlertingConfig{}, TargetPolicy{},
		logs.fn, WithBriefing(b))
	if err != nil || res != (ReconcileResult{}) {
		t.Fatalf("reconcile = %+v, %v; want nothing written", res, err)
	}
	out := logs.joined()
	if !strings.Contains(out, "briefing.slack_webhook_url") || strings.Contains(out,
		"SECRETPATH") {
		t.Fatalf("log = %q, want the key named and no secret path", out)
	}
}
