package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestLogwatchRuntimeUsesExplicitPathAndReusesOwner(t *testing.T) {
	prepareMetaGlobals(t)
	cfg.LogWatch = config.LogWatchConfig{Enabled: true, LogDirectory: t.TempDir(),
		Format: "jsonlog", PollIntervalMs: 5, MaxLineLenBytes: 65536}
	logPath := filepath.Join(cfg.LogWatch.LogDirectory, "postgresql.json")
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolvedLogWatchConfig(nil)
	if err != nil || resolved.LogDirectory != cfg.LogWatch.LogDirectory ||
		resolved.Format != "jsonlog" || logwatchPollInterval() != 5*time.Millisecond {
		t.Fatalf("explicit config ignored: %+v err=%v", resolved, err)
	}
	watcher := ensureStandaloneLogWatcher(nil, nil)
	if watcher == nil {
		t.Fatal("explicit accessible log directory did not start watcher")
	}
	t.Cleanup(watcher.Stop)
	if ensureStandaloneLogWatcher(watcher, nil) != watcher {
		t.Fatal("existing watcher was replaced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { runStandaloneLogDrain(ctx, watcher, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled log drain did not terminate")
	}
	cfg.LogWatch.Enabled = false
	if ensureStandaloneLogWatcher(nil, nil) != nil {
		t.Fatal("disabled watcher started")
	}
	cfg.LogWatch.PollIntervalMs = 0
	if logwatchPollInterval() != time.Second {
		t.Fatal("zero poll interval lost bounded default")
	}
}

func TestLogwatchRuntimeRejectsUnreadableConfiguredPath(t *testing.T) {
	prepareMetaGlobals(t)
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.LogWatch = config.LogWatchConfig{Enabled: true, LogDirectory: path, Format: "jsonlog"}
	if ensureStandaloneLogWatcher(nil, nil) != nil {
		t.Fatal("file path was accepted as a log directory")
	}
}

func TestAlertRoutingRetainsOnlyExplicitKnownDestinations(t *testing.T) {
	c := config.DefaultConfig()
	c.Alerting.SlackWebhookURL = "http://127.0.0.1:1/slack"
	c.Alerting.PagerDutyRoutingKey = "fixture-only"
	c.Alerting.Webhooks = []config.WebhookConfig{{Name: "local", URL: "http://127.0.0.1:1"}}
	c.Alerting.Routes = []config.AlertRoute{
		{Severity: "critical", Channels: []string{"slack", "pagerduty", "webhook:local", "missing"}},
		{Severity: "warning", Channels: []string{"missing"}},
	}
	routes := buildAlertRoutes(c, nil)
	if len(routes["critical"]) != 3 || len(routes["warning"]) != 0 {
		t.Fatalf("unknown destination admitted or configured destination lost: %+v", routes)
	}
	if len(buildAlertRoutes(config.DefaultConfig(), nil)) != 0 {
		t.Fatal("default config produced unconfigured notification destinations")
	}
}
