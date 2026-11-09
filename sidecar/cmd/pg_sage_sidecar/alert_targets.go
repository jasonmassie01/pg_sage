package main

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// configTargetPolicy is the notification target policy a config declares
// (G7-B21). Notify senders and alerting channels share it; fleet mode has
// one policy for every database.
func configTargetPolicy(c *config.Config) notify.TargetPolicy {
	return notify.TargetPolicy{
		AllowPrivate: c != nil && c.NotificationPolicy.AllowPrivateTargets,
	}
}

// validateAlertTargets refuses an enabled alerting block whose Slack or
// webhook URL the notification target policy blocks (alerting-11). It is a
// static check; the policy client re-checks each resolved address at dial
// time. It lives here because config cannot import notify.
func validateAlertTargets(c *config.Config) error {
	if c == nil || !c.Alerting.Enabled {
		return nil
	}
	policy := configTargetPolicy(c)
	if err := checkAlertTarget(
		policy, "alerting.slack_webhook_url", c.Alerting.SlackWebhookURL,
	); err != nil {
		return err
	}
	for i, wh := range c.Alerting.Webhooks {
		if err := checkAlertTarget(policy, webhookURLKey(i, wh.Name), wh.URL); err != nil {
			return err
		}
	}
	return nil
}

// checkAlertTarget checks one URL; an empty URL configures no channel. The
// error names the key, never the URL, whose path carries the secret.
func checkAlertTarget(policy notify.TargetPolicy, key, rawURL string) error {
	if rawURL == "" {
		return nil
	}
	if err := policy.ValidateURL(rawURL); err != nil {
		return fmt.Errorf("%s: %w; an on-prem relay over plain http or on a "+
			"private address needs notification_policy.allow_private_targets: true "+
			"(link-local and metadata hosts stay blocked)", key, err)
	}
	return nil
}

func webhookURLKey(i int, name string) string {
	return fmt.Sprintf("alerting.webhooks[%d].url (webhook %q)", i, name)
}

func logBlockedAlertTarget(logFn func(string, string, ...any), err error) {
	if logFn == nil {
		return
	}
	logFn("ERROR", "alerting: channel not built: %v", err)
}
