package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-11: YAML alerting channels obey the notification target policy
// (G7-B21) at startup, on reload, when routes are built and at dial time.
// No concurrency test here: validateAlertTargets and buildAlertRoutes read
// one config and build fresh values; the shared policy client is exercised
// concurrently in internal/alerting/target_test.go.

func targetConfig(webhookURL string, allowPrivate bool) *config.Config {
	c := webhookConfig(config.AlertRoute{Severity: "critical", Channels: []string{"myapp"}})
	c.Alerting.Webhooks[0].URL = webhookURL
	c.NotificationPolicy.AllowPrivateTargets = allowPrivate
	return c
}

func listenerCount(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var conns, hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns, &hits
}

func sendCritical(t *testing.T, ch alerting.Channel) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ch.Send(ctx, alerting.Alert{
		Findings:  []alerting.AlertFinding{{ID: 1, Title: "t", Severity: "critical"}},
		Severity:  "critical",
		Timestamp: time.Now(),
	})
}

func TestValidateAlertTargets_LinkLocalWebhookRefused(t *testing.T) {
	for _, allow := range []bool{false, true} {
		c := targetConfig("http://169.254.169.254/latest", allow)
		err := validateAlertTargets(c)
		if !errors.Is(err, notify.ErrTargetBlocked) {
			t.Fatalf("allow_private=%v: err = %v, want ErrTargetBlocked", allow, err)
		}
		if !strings.Contains(err.Error(), "alerting.webhooks[0].url") ||
			!strings.Contains(err.Error(), "myapp") {
			t.Fatalf("err = %q, want the key path and webhook name", err)
		}
	}
}

func TestValidateAlertTargets_PrivateNeedsAllowPrivate(t *testing.T) {
	if err := validateAlertTargets(targetConfig("http://10.0.0.5/hook", false)); !errors.Is(
		err, notify.ErrTargetBlocked) {
		t.Fatalf("private webhook without allow_private_targets: err = %v, want blocked", err)
	} else if !strings.Contains(err.Error(), "allow_private_targets") {
		t.Fatalf("err = %q, want the allow_private_targets hint", err)
	}
	if err := validateAlertTargets(targetConfig("http://10.0.0.5/hook", true)); err != nil {
		t.Fatalf("private webhook with allow_private_targets refused: %v", err)
	}
}

func TestValidateAlertTargets_SlackURLChecked(t *testing.T) {
	c := slackRoutedConfig()
	c.Alerting.SlackWebhookURL = "https://169.254.169.254/services/x"
	err := validateAlertTargets(c)
	if !errors.Is(err, notify.ErrTargetBlocked) ||
		!strings.Contains(err.Error(), "alerting.slack_webhook_url") {
		t.Fatalf("err = %v, want a refusal naming alerting.slack_webhook_url", err)
	}
	if strings.Contains(err.Error(), "/services/x") {
		t.Fatalf("err %q echoes the secret URL path", err)
	}
	c.Alerting.SlackWebhookURL = "https://hooks.slack.com/services/T/B/x"
	if err := validateAlertTargets(c); err != nil {
		t.Fatalf("public Slack URL refused: %v", err)
	}
}

func TestValidateAlertTargets_EmptyAndDisabled(t *testing.T) {
	c := slackRoutedConfig()
	c.Alerting.SlackWebhookURL = ""
	if err := validateAlertTargets(c); err != nil {
		t.Fatalf("empty Slack URL: %v (ValidateAlerting owns missing secrets)", err)
	}
	c = targetConfig("http://169.254.169.254/latest", false)
	c.Alerting.Enabled = false
	if err := validateAlertTargets(c); err != nil {
		t.Fatalf("disabled alerting checked: %v", err)
	}
	if err := validateAlertTargets(nil); err != nil {
		t.Fatalf("nil config: %v", err)
	}
}

func TestBuildAlertRoutes_BlockedTargetBuildsNoChannelAndNeverDials(t *testing.T) {
	srv, conns, _ := listenerCount(t)
	var log routeLog
	routes := buildAlertRoutes(targetConfig(srv.URL+"/hook", false), log.logFn)
	if n := len(routes["critical"]); n != 0 {
		t.Fatalf("critical routes = %d channels, want the loopback webhook dropped", n)
	}
	if conns.Load() != 0 {
		t.Fatalf("listener saw %d connections, want none", conns.Load())
	}
	if len(log.lines) != 1 || !strings.HasPrefix(log.lines[0], "ERROR") ||
		!strings.Contains(log.lines[0], "alerting.webhooks[0].url") {
		t.Fatalf("log = %q, want one ERROR naming the blocked key", log.lines)
	}
	routes = buildAlertRoutes(targetConfig("http://169.254.169.254/latest", true), nil)
	if n := len(routes["critical"]); n != 0 {
		t.Fatalf("link-local webhook built %d channels with allow_private_targets", n)
	}
}

func TestBuildAlertRoutes_AllowPrivateDelivers(t *testing.T) {
	srv, _, hits := listenerCount(t)
	routes := buildAlertRoutes(targetConfig(srv.URL+"/hook", true), nil)
	if len(routes["critical"]) != 1 {
		t.Fatalf("critical routes = %d channels, want the webhook", len(routes["critical"]))
	}
	if err := sendCritical(t, routes["critical"][0]); err != nil {
		t.Fatalf("send through allow-private policy: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("relay handled %d requests, want 1", hits.Load())
	}
}

// Built channels connect through the policy client: an allowed relay that
// redirects to the metadata address is refused by the redirect check, not
// followed by a default client.
func TestBuildAlertRoutes_ChannelFollowsPolicyOnRedirect(t *testing.T) {
	var hits atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	t.Cleanup(relay.Close)
	routes := buildAlertRoutes(targetConfig(relay.URL+"/hook", true), nil)
	if len(routes["critical"]) != 1 {
		t.Fatalf("critical routes = %d channels, want the relay webhook", len(routes["critical"]))
	}
	err := sendCritical(t, routes["critical"][0])
	if !errors.Is(err, notify.ErrTargetBlocked) {
		t.Fatalf("redirect to metadata: err = %v, want ErrTargetBlocked", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("relay handled %d requests, want 1", hits.Load())
	}
}

func TestBuildAlertRoutes_EmptyURLBuildsNoChannel(t *testing.T) {
	c := slackRoutedConfig()
	c.Alerting.SlackWebhookURL = ""
	var log routeLog
	if n := len(buildAlertRoutes(c, log.logFn)["critical"]); n != 0 {
		t.Fatalf("empty Slack URL built %d channels", n)
	}
	if len(log.lines) != 1 || !strings.HasPrefix(log.lines[0], "WARN") {
		t.Fatalf("log = %q, want only the unrouted-channel WARN", log.lines)
	}
}

func TestStartupRefusesBlockedAlertTarget(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = slackRoutedConfig()
	cfg.Alerting.SlackWebhookURL = "http://169.254.169.254/latest/meta-data"
	configController = nil
	err := initializeConfigController(nil)
	if !errors.Is(err, notify.ErrTargetBlocked) ||
		!strings.Contains(err.Error(), "alerting.slack_webhook_url") {
		t.Fatalf("err = %v, want startup refused for the link-local Slack URL", err)
	}
	if configController != nil {
		t.Fatal("controller built for a blocked alert target")
	}
}

func TestWatchedConfigRefusesBlockedAlertTarget(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	oldMeta, oldPool := globalMetaState, pool
	t.Cleanup(func() { globalMetaState, pool = oldMeta, oldPool })
	globalMetaState, pool = nil, nil
	cfg = config.DefaultConfig()
	cfg.Mode = config.ModeMeta
	configController = nil
	fleetMgr = nil
	if err := ensureConfigController(); err != nil {
		t.Fatalf("ensure controller: %v", err)
	}
	before := configController.Desired().Generation
	updated := targetConfig("http://169.254.169.254/latest", true)
	updated.Mode = config.ModeMeta
	if err := applyWatchedConfig(updated); !errors.Is(err, notify.ErrTargetBlocked) {
		t.Fatalf("watched reload err = %v, want ErrTargetBlocked", err)
	}
	if got := configController.Desired().Generation; got != before {
		t.Fatalf("desired generation = %d, want %d (reload refused)", got, before)
	}
}

// The Slack channel gets the same build-time check as webhooks: a
// link-local Slack URL builds nothing, even with private targets allowed.
func TestBuildAlertRoutes_BlockedSlackURLBuildsNoChannel(t *testing.T) {
	c := slackRoutedConfig()
	c.Alerting.SlackWebhookURL = "https://169.254.169.254/services/T/B/secret"
	c.NotificationPolicy.AllowPrivateTargets = true
	var log routeLog
	if n := len(buildAlertRoutes(c, log.logFn)["critical"]); n != 0 {
		t.Fatalf("link-local Slack URL built %d channels", n)
	}
	if len(log.lines) != 1 || !strings.HasPrefix(log.lines[0], "ERROR") ||
		!strings.Contains(log.lines[0], "alerting.slack_webhook_url") ||
		strings.Contains(log.lines[0], "secret") {
		t.Fatalf("log = %q, want one ERROR naming the key without the URL", log.lines)
	}
	c.Alerting.SlackWebhookURL = "https://hooks.slack.com/services/T/B/x"
	routes := buildAlertRoutes(c, nil)["critical"]
	if len(routes) != 1 || routes[0].Name() != "slack" {
		t.Fatalf("public Slack URL routes = %v, want the slack channel", routes)
	}
}
