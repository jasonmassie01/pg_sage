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
	cfg.RCA.Enabled = true
	logPath := filepath.Join(cfg.LogWatch.LogDirectory, "postgresql.json")
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolvedLogWatchConfig(context.Background(), nil)
	if err != nil || resolved.LogDirectory != cfg.LogWatch.LogDirectory ||
		resolved.Format != "jsonlog" || logwatchPollInterval() != 5*time.Millisecond {
		t.Fatalf("explicit config ignored: %+v err=%v", resolved, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set := newLogFanoutSet()
	fanout := set.forCluster(ctx, "local:5432", nil)
	if fanout == nil {
		t.Fatal("explicit accessible log directory did not start watcher")
	}
	if set.forCluster(ctx, "local:5432", nil) != fanout {
		t.Fatal("existing cluster watcher was replaced")
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	done := make(chan struct{})
	go func() { runFanoutDrain(stopped, "local:5432", fanout, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled log drain did not terminate")
	}
	cfg.LogWatch.Enabled = false
	if (&databaseRuntime{}).clusterLogFanout() != nil {
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
	set := newLogFanoutSet()
	if set.forCluster(context.Background(), "local:5432", nil) != nil {
		t.Fatal("file path was accepted as a log directory")
	}
	if len(set.fanouts) != 0 {
		t.Fatal("a failed watcher was cached for the cluster")
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
