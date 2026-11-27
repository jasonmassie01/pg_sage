package notify

import (
	"slices"

	"github.com/pg-sage/sidecar/internal/config"
)

// The briefing's channel (ux-02b): briefing.channels with "slack" and
// briefing.slack_webhook_url become one config-managed Slack channel with a
// digest_ready rule, so the digest goes through Notifications like every
// event and the briefing has no Slack post path of its own.

// BriefingChannelName is the managed channel briefing.slack_webhook_url
// becomes.
var BriefingChannelName = ConfigChannelName("slack", "briefing")

// ReconcileOption adds a config block other than alerting: to a reconcile.
type ReconcileOption func(*reconcileInput)

type reconcileInput struct {
	briefing *config.BriefingConfig
}

// WithBriefing reconciles the briefing's Slack target too. Every caller
// that reconciles the running config passes it: a reconcile without it
// removes the briefing channel like any config row no block asks for.
func WithBriefing(b config.BriefingConfig) ReconcileOption {
	return func(in *reconcileInput) { in.briefing = &b }
}

// briefingState is the briefing block's channel and its digest rule, or
// nothing when the block asks for no Slack delivery or the target policy
// refuses its URL (logged by the key, never the URL).
func briefingState(
	b *config.BriefingConfig, policy TargetPolicy, logFn func(string, string, ...any),
) ([]ManagedChannel, []desiredRule) {
	if b == nil || b.SlackWebhookURL == "" || !slices.Contains(b.Channels, "slack") {
		return nil, nil
	}
	if err := policy.ValidateURL(b.SlackWebhookURL); err != nil {
		logRefusedTarget(logFn, "briefing.slack_webhook_url", err)
		return nil, nil
	}
	ch := managedChannel("slack", "briefing",
		map[string]string{"webhook_url": b.SlackWebhookURL})
	rule := desiredRule{channel: ch.Name, event: EventDigestReady, minSeverity: "info",
		hash: hashOf(ch.Name, EventDigestReady, "info")}
	return []ManagedChannel{ch}, []desiredRule{rule}
}
