package main

import (
	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
)

// buildAlertRoutes constructs channel instances and severity routing
// from the alerting config.
func buildAlertRoutes(
	c *config.Config,
	logFn func(string, string, ...any),
) map[string][]alerting.Channel {
	channels := buildAlertChannels(c, logFn)
	routes := make(map[string][]alerting.Channel)
	for _, r := range c.Alerting.Routes {
		routed := make(map[string]bool, len(r.Channels))
		for _, chName := range r.Channels {
			key, ok := c.Alerting.ResolveAlertChannel(chName)
			if !ok {
				warnUnroutedChannel(logFn, r.Severity, chName)
				continue
			}
			ch, built := channels[key]
			if built && !routed[key] {
				routed[key] = true
				routes[r.Severity] = append(routes[r.Severity], ch)
			}
		}
	}
	return routes
}

// buildAlertChannels builds the configured channels on one client under
// the notification target policy (alerting-11). A target the policy
// refuses builds no channel and is logged; the policy client re-checks
// every dialled address and redirect.
func buildAlertChannels(
	c *config.Config, logFn func(string, string, ...any),
) map[string]alerting.Channel {
	policy := configTargetPolicy(c)
	client := policy.HTTPClient(alerting.HTTPTimeout)
	channels := make(map[string]alerting.Channel)
	if u := c.Alerting.SlackWebhookURL; u != "" {
		if err := checkAlertTarget(policy, "alerting.slack_webhook_url", u); err != nil {
			logBlockedAlertTarget(logFn, err)
		} else {
			channels["slack"] = alerting.NewSlack(u, client, logFn)
		}
	}
	if c.Alerting.PagerDutyRoutingKey != "" {
		channels["pagerduty"] = alerting.NewPagerDuty(
			c.Alerting.PagerDutyRoutingKey, client, logFn,
		)
	}
	for i, wh := range c.Alerting.Webhooks {
		if err := checkAlertTarget(policy, webhookURLKey(i, wh.Name), wh.URL); err != nil {
			logBlockedAlertTarget(logFn, err)
			continue
		}
		channels["webhook:"+wh.Name] = alerting.NewWebhook(
			wh.Name, wh.URL, wh.Headers, client, logFn,
		)
	}
	return channels
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
