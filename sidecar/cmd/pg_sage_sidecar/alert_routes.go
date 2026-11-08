package main

import (
	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// registerNotifySenders wires the built-in notification senders onto a
// dispatcher. Without it, the executor/analyzer event path dispatched to
// a sender-less dispatcher and every notification silently no-op'd with
// "no sender for type" (F1).
func registerNotifySenders(d *notify.Dispatcher) {
	policy := notificationTargetPolicy()
	d.RegisterSender(notify.NewSlackSenderWithPolicy(policy))
	d.RegisterSender(notify.NewEmailSenderWithPolicy(policy))
	d.RegisterSender(notify.NewPagerDutySender())
	d.RegisterSender(notify.NewTelegramSenderWithPolicy(policy))
}

// buildAlertRoutes constructs channel instances and severity routing
// from the alerting config.
func buildAlertRoutes(
	c *config.Config,
	logFn func(string, string, ...any),
) map[string][]alerting.Channel {
	channels := make(map[string]alerting.Channel)
	if c.Alerting.SlackWebhookURL != "" {
		channels["slack"] = alerting.NewSlack(
			c.Alerting.SlackWebhookURL, logFn,
		)
	}
	if c.Alerting.PagerDutyRoutingKey != "" {
		channels["pagerduty"] = alerting.NewPagerDuty(
			c.Alerting.PagerDutyRoutingKey, logFn,
		)
	}
	for _, wh := range c.Alerting.Webhooks {
		channels["webhook:"+wh.Name] = alerting.NewWebhook(
			wh.Name, wh.URL, wh.Headers, logFn,
		)
	}

	routes := make(map[string][]alerting.Channel)
	for _, r := range c.Alerting.Routes {
		routed := make(map[string]bool, len(r.Channels))
		for _, chName := range r.Channels {
			key, ok := c.Alerting.ResolveAlertChannel(chName)
			if !ok {
				warnUnroutedChannel(logFn, r.Severity, chName)
				continue
			}
			if !routed[key] {
				routed[key] = true
				routes[r.Severity] = append(routes[r.Severity], channels[key])
			}
		}
	}
	return routes
}

// warnUnroutedChannel reports a route channel that resolves to nothing.
// Startup validation refuses these; a hot-reloaded secret can still empty
// a channel, and that must not go silent (alerting-08).
func warnUnroutedChannel(logFn func(string, string, ...any), severity, name string) {
	if logFn == nil {
		return
	}
	logFn("WARN", "alerting: %s route channel %q is not configured; nothing is sent to it",
		severity, name)
}
