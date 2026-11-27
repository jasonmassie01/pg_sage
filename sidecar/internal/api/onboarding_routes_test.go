package api

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// alerting-08: onboarding calls notifications configured only when
// alerting is on and at least one route reaches a usable channel. Without
// that the answer comes from the notification channels table, which only
// an admin with a control database can read (nil otherwise).
// No concurrency test: Notifications reads one config without writing it.

func alertingReader(a config.AlertingConfig) *fleetOnboardingReader {
	cfg := config.DefaultConfig()
	cfg.Alerting = a
	return &fleetOnboardingReader{cfg: cfg}
}

func adminCtx() context.Context {
	return context.WithValue(context.Background(), userContextKey, testAdminUser())
}

func TestOnboardingNotifications_UnresolvableRoutesAreNotConfigured(t *testing.T) {
	for name, a := range map[string]config.AlertingConfig{
		"case typo": {Enabled: true, SlackWebhookURL: "https://hooks.slack.example.com/x",
			Routes: []config.AlertRoute{{Severity: "critical", Channels: []string{"Slack"}}}},
		"no routes": {Enabled: true, SlackWebhookURL: "https://hooks.slack.example.com/x",
			PagerDutyRoutingKey: "k"},
		"webhook not routed": {Enabled: true,
			Webhooks: []config.WebhookConfig{{Name: "a", URL: "https://hooks.example.com"}},
			Routes:   []config.AlertRoute{{Severity: "critical", Channels: []string{"b"}}}},
	} {
		if got := alertingReader(a).Notifications(adminCtx()); got != nil && *got {
			t.Errorf("%s: Notifications = true for alerting that sends nothing", name)
		}
	}
}

func TestOnboardingNotifications_UsableRouteIsConfigured(t *testing.T) {
	a := config.AlertingConfig{Enabled: true,
		Webhooks: []config.WebhookConfig{{Name: "ops", URL: "https://hooks.example.com"}},
		Routes:   []config.AlertRoute{{Severity: "warning", Channels: []string{"ops"}}}}
	got := alertingReader(a).Notifications(context.Background())
	if got == nil || !*got {
		t.Fatalf("Notifications = %v, want true for a routed webhook", got)
	}
}

func TestOnboardingNotifications_DisabledAlertingIsNotConfigured(t *testing.T) {
	a := config.AlertingConfig{SlackWebhookURL: "https://hooks.slack.example.com/x",
		Routes: []config.AlertRoute{{Severity: "critical", Channels: []string{"slack"}}}}
	if got := alertingReader(a).Notifications(adminCtx()); got != nil {
		t.Fatalf("Notifications = %v, want nil (unknown without a control database)", *got)
	}
}
