package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// alerting-08 review: config.Load checks the file before persisted
// overrides exist, so a slack route whose URL was pasted in the dashboard
// must load; startup checks the effective config once overrides apply.
// No concurrency test: startup runs once, before any goroutine reads cfg.

func slackRoutedConfig() *config.Config {
	c := config.DefaultConfig()
	c.Alerting.Enabled = true
	c.Alerting.Routes = []config.AlertRoute{
		{Severity: "critical", Channels: []string{"slack"}},
	}
	return c
}

func TestStartupRefusesEffectiveAlertingWithoutSecret(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = slackRoutedConfig()
	configController = nil
	err := initializeConfigController(nil)
	if err == nil || !strings.Contains(err.Error(), "alerting.slack_webhook_url") ||
		!strings.Contains(err.Error(), "alerting.routes[0].channels[0]") {
		t.Fatalf("err = %v, want a refusal naming the route and the Slack URL key", err)
	}
	if configController != nil {
		t.Fatal("controller built for a config that would send nothing")
	}
}

func TestStartupAcceptsEffectiveAlertingWithSecret(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = slackRoutedConfig()
	cfg.Alerting.SlackWebhookURL = "https://hooks.slack.example.com/x"
	configController = nil
	if err := initializeConfigController(nil); err != nil {
		t.Fatalf("initializeConfigController: %v", err)
	}
	if configController == nil {
		t.Fatal("no controller built for a valid alerting block")
	}
}

func TestStartupDisabledAlertingIsNotChecked(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = slackRoutedConfig()
	cfg.Alerting.Enabled = false
	configController = nil
	if err := initializeConfigController(nil); err != nil {
		t.Fatalf("disabled alerting refused at startup: %v", err)
	}
}

// Integration: the Slack URL lives only in sage.config (saved from the
// Settings page). Startup applies it and accepts the YAML route.
func TestStartupAcceptsSlackURLFromPersistedOverride(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	pool := preflightRuntimePool(t)
	ctx := context.Background()
	configs := store.NewConfigStore(pool)
	const url = "https://hooks.slack.example.com/persisted"
	if err := configs.SetOverride(ctx, "alerting.slack_webhook_url", url, 0,
		fixtureAdminID(t, pool)); err != nil {
		t.Fatalf("persist slack URL: %v", err)
	}
	t.Cleanup(func() {
		if err := configs.DeleteOverride(ctx, "alerting.slack_webhook_url", 0); err != nil {
			t.Errorf("delete override: %v", err)
		}
	})
	cfg = slackRoutedConfig()
	configController = nil
	if err := initializeConfigController(pool); err != nil {
		t.Fatalf("startup refused a Slack URL supplied by an override: %v", err)
	}
	if cfg.Alerting.SlackWebhookURL != url || !cfg.Alerting.HasUsableRoute() {
		t.Fatalf("effective alerting = %+v, want the persisted URL and a usable route",
			cfg.Alerting)
	}
}
