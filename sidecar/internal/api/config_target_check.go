package api

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// configTargetPolicy is the notification target policy the config
// declares (G7-B21); it is YAML-only, so an API write cannot widen it.
func configTargetPolicy(cfg *config.Config) notify.TargetPolicy {
	return notify.TargetPolicy{
		AllowPrivate: cfg != nil && cfg.NotificationPolicy.AllowPrivateTargets,
	}
}

// checkConfigTarget refuses an alerting Slack URL the policy blocks before
// it is persisted or hot-reloaded (alerting-11). An empty value clears the
// channel. The error never echoes the URL, whose path is the secret.
func checkConfigTarget(key, value string, policy notify.TargetPolicy) error {
	if key != "alerting.slack_webhook_url" || value == "" {
		return nil
	}
	if err := policy.ValidateURL(value); err != nil {
		return fmt.Errorf("invalid value: %w; a private relay needs "+
			"notification_policy.allow_private_targets: true in the YAML config", err)
	}
	return nil
}
