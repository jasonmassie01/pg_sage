package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// alerting-08: routing resolves channel names exactly as config
// validation does, so a route validation accepts always delivers.
// No concurrency test: buildAlertRoutes builds a fresh map per call.

type routeLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *routeLog) logFn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func webhookConfig(routes ...config.AlertRoute) *config.Config {
	c := config.DefaultConfig()
	c.Alerting.Enabled = true
	c.Alerting.Webhooks = []config.WebhookConfig{
		{Name: "myapp", URL: "http://127.0.0.1:1/hook"},
	}
	c.Alerting.Routes = routes
	// The loopback fixture URL models an on-prem relay (alerting-11).
	c.NotificationPolicy.AllowPrivateTargets = true
	return c
}

func TestBuildAlertRoutes_BareWebhookNameRoutes(t *testing.T) {
	c := webhookConfig(config.AlertRoute{Severity: "critical", Channels: []string{"myapp"}})
	routes := buildAlertRoutes(c, nil)
	if len(routes["critical"]) != 1 {
		t.Fatalf("critical routes = %d channels, want the myapp webhook", len(routes["critical"]))
	}
	if got := routes["critical"][0].Name(); !strings.Contains(got, "myapp") {
		t.Fatalf("routed channel %q, want the myapp webhook", got)
	}
}

func TestBuildAlertRoutes_AliasAndPrefixDeliverOnce(t *testing.T) {
	c := webhookConfig(config.AlertRoute{Severity: "warning",
		Channels: []string{"myapp", "webhook:myapp"}})
	if got := len(buildAlertRoutes(c, nil)["warning"]); got != 1 {
		t.Fatalf("warning routes = %d channels, want one webhook delivery", got)
	}
}

func TestBuildAlertRoutes_UnresolvableChannelIsLogged(t *testing.T) {
	c := webhookConfig(
		config.AlertRoute{Severity: "critical", Channels: []string{"Slack", "myapp"}},
	)
	var log routeLog
	routes := buildAlertRoutes(c, log.logFn)
	if len(routes["critical"]) != 1 {
		t.Fatalf("critical routes = %d channels, want only myapp", len(routes["critical"]))
	}
	if len(log.lines) != 1 || !strings.HasPrefix(log.lines[0], "WARN") ||
		!strings.Contains(log.lines[0], `"Slack"`) ||
		!strings.Contains(log.lines[0], "critical") {
		t.Fatalf("log = %q, want one WARN naming the critical route's \"Slack\"", log.lines)
	}
}

func TestBuildAlertRoutes_EmptyRoutesBuildNothing(t *testing.T) {
	var log routeLog
	if routes := buildAlertRoutes(webhookConfig(), log.logFn); len(routes) != 0 {
		t.Fatalf("routes = %+v, want none without alerting.routes", routes)
	}
	if len(log.lines) != 0 {
		t.Fatalf("log = %q, want nothing logged for an empty route list", log.lines)
	}
}
