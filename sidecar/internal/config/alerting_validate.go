package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // validation loads the same zones the throttle does
)

// alertSeverities are the finding severities a route can name.
var alertSeverities = map[string]bool{"critical": true, "warning": true, "info": true}

const alertWebhookPrefix = "webhook:"

// ResolveAlertChannel maps a route channel name to the key of a usable
// channel: "slack" with a webhook URL, "pagerduty" with a routing key, or
// "webhook:<name>" for a webhook with a URL. A bare webhook name is an
// alias of "webhook:<name>"; "slack" and "pagerduty" always mean the
// built-in channels. Routing and validation both use it, so a route that
// validates always delivers.
func (a *AlertingConfig) ResolveAlertChannel(name string) (string, bool) {
	switch {
	case name == "slack" && a.SlackWebhookURL != "",
		name == "pagerduty" && a.PagerDutyRoutingKey != "":
		return name, true
	case name == "slack", name == "pagerduty":
		return "", false
	}
	hook := strings.TrimPrefix(name, alertWebhookPrefix)
	if hook == "" {
		return "", false
	}
	for _, wh := range a.Webhooks {
		if wh.Name == hook && wh.URL != "" {
			return alertWebhookPrefix + hook, true
		}
	}
	return "", false
}

// HasUsableRoute reports whether at least one route reaches a usable
// channel.
func (a *AlertingConfig) HasUsableRoute() bool {
	for _, r := range a.Routes {
		for _, ch := range r.Channels {
			if _, ok := a.ResolveAlertChannel(ch); ok {
				return true
			}
		}
	}
	return false
}

// QuietHourMinute converts "HH" or "HH:MM" to the minute of day; -1 when
// empty or invalid. The alert throttle parses quiet hours with it.
func QuietHourMinute(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	parts := strings.SplitN(s, ":", 2)
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return -1
	}
	minute := 0
	if len(parts) == 2 {
		minute, err = strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return -1
		}
	}
	return h*60 + minute
}

// validateFileConfig validates a configuration as Load reads it (YAML,
// env, secret files, flags): everything validate checks, plus the
// alerting block's structure. The Slack URL and PagerDuty key are not
// required yet, because a persisted dashboard override can still supply
// them; ValidateAlerting checks the effective config once overrides are
// applied. API candidates skip the alerting check so that a persisted
// alerting.enabled override never blocks an unrelated settings change
// such as lowering trust; routing logs an unroutable channel.
func (c *Config) validateFileConfig() error {
	if err := c.validate(); err != nil {
		return err
	}
	return c.Alerting.validateBlock(false)
}

// validateEffective validates a reloaded candidate that already carries
// the persisted overrides: validate plus the full alerting check.
func (c *Config) validateEffective() error {
	if err := c.validate(); err != nil {
		return err
	}
	return c.ValidateAlerting()
}

// ValidateAlerting refuses an enabled alerting block that would send
// nothing or not what it says (alerting-08), including a slack or
// pagerduty route without its URL or key. Run it on the effective config,
// after persisted overrides. A disabled block is not validated. Fleet mode
// shares this one block.
func (c *Config) ValidateAlerting() error {
	return c.Alerting.validateBlock(true)
}

// validateBlock checks an enabled block; requireSecrets also demands the
// Slack URL and PagerDuty key behind the routes that name them.
func (a *AlertingConfig) validateBlock(requireSecrets bool) error {
	if err := a.validateEngine(); err != nil {
		return err
	}
	if !a.Enabled {
		return nil
	}
	if err := a.validateWebhooks(); err != nil {
		return err
	}
	if err := a.validateRoutes(requireSecrets); err != nil {
		return err
	}
	if err := a.validateRepeat(); err != nil {
		return err
	}
	return a.validateSchedule()
}

func (a *AlertingConfig) validateWebhooks() error {
	seen := make(map[string]bool, len(a.Webhooks))
	for i, wh := range a.Webhooks {
		switch {
		case wh.Name == "":
			return fmt.Errorf("alerting.webhooks[%d].name is empty; routes name a "+
				"webhook by it", i)
		case seen[wh.Name]:
			return fmt.Errorf("alerting.webhooks[%d].name %q is a duplicate; webhook "+
				"names must be unique", i, wh.Name)
		case wh.URL == "":
			return fmt.Errorf("alerting.webhooks[%d].url is empty for webhook %q", i, wh.Name)
		}
		seen[wh.Name] = true
	}
	return nil
}

func (a *AlertingConfig) validateRoutes(requireSecrets bool) error {
	if len(a.Routes) == 0 {
		return fmt.Errorf("alerting.enabled is true but alerting.routes is empty, so no " +
			"alert would be sent; add a route such as {severity: critical, channels: [slack]}")
	}
	for i, r := range a.Routes {
		if !alertSeverities[r.Severity] {
			return fmt.Errorf("alerting.routes[%d].severity %q is not one of "+
				"critical, warning, info", i, r.Severity)
		}
		if len(r.Channels) == 0 {
			return fmt.Errorf("alerting.routes[%d].channels is empty; list slack, "+
				"pagerduty or a webhook name", i)
		}
		for j, ch := range r.Channels {
			if a.channelAccepted(ch, requireSecrets) {
				continue
			}
			return fmt.Errorf("alerting.routes[%d].channels[%d] %q %s",
				i, j, ch, a.unresolvedReason(ch))
		}
	}
	return nil
}

// channelAccepted reports whether a route channel passes: it resolves, or
// it is a built-in whose secret may still come from a persisted override.
func (a *AlertingConfig) channelAccepted(name string, requireSecrets bool) bool {
	if _, ok := a.ResolveAlertChannel(name); ok {
		return true
	}
	return !requireSecrets && (name == "slack" || name == "pagerduty")
}

// unresolvedReason says why ResolveAlertChannel refused name.
func (a *AlertingConfig) unresolvedReason(name string) string {
	switch name {
	case "slack":
		return "needs alerting.slack_webhook_url"
	case "pagerduty":
		return "needs alerting.pagerduty_routing_key"
	}
	reason := "is not a configured channel; use slack, pagerduty, or a name from " +
		"alerting.webhooks (bare or as webhook:<name>)"
	candidates := []string{"slack", "pagerduty"}
	for _, wh := range a.Webhooks {
		candidates = append(candidates, wh.Name)
	}
	bare := strings.TrimPrefix(name, alertWebhookPrefix)
	for _, cand := range candidates {
		if strings.EqualFold(cand, bare) {
			return fmt.Sprintf("%s (did you mean %q? names are case-sensitive)", reason, cand)
		}
	}
	return reason
}

func (a *AlertingConfig) validateSchedule() error {
	for _, f := range []struct{ key, val string }{
		{"quiet_hours_start", a.QuietHoursStart}, {"quiet_hours_end", a.QuietHoursEnd},
	} {
		if strings.TrimSpace(f.val) != "" && QuietHourMinute(f.val) < 0 {
			return fmt.Errorf("alerting.%s %q is not HH:MM (00:00 to 23:59)", f.key, f.val)
		}
	}
	start := strings.TrimSpace(a.QuietHoursStart) != ""
	if start != (strings.TrimSpace(a.QuietHoursEnd) != "") {
		return fmt.Errorf("alerting.quiet_hours_start and alerting.quiet_hours_end must " +
			"both be set or both be empty")
	}
	if a.Timezone != "" {
		if _, err := time.LoadLocation(a.Timezone); err != nil {
			return fmt.Errorf("alerting.timezone %q does not load: %w", a.Timezone, err)
		}
	}
	return nil
}
