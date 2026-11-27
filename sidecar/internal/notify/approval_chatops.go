package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// slackHookHost is the only host a Slack interaction's response_url may
// name; private relays are allowed only with an AllowPrivate policy.
const slackHookHost = "hooks.slack.com"

// Reply answers a decision made in Slack on the interaction's
// response_url, without replacing the approval message.
func (s *SlackSender) Reply(ctx context.Context, ch Channel, a chatops.Action,
	text string) error {
	if err := s.policy.ValidateURL(a.ResponseURL); err != nil {
		return fmt.Errorf("slack channel %q reply: %w", ch.Name, err)
	}
	u, err := url.Parse(a.ResponseURL)
	if err != nil || (!s.policy.AllowPrivate && u.Hostname() != slackHookHost) {
		return fmt.Errorf("slack channel %q reply: %w: response_url is not a Slack hook",
			ch.Name, ErrTargetBlocked)
	}
	payload, err := json.Marshal(map[string]any{"text": TruncateRunes(text,
		slackSectionMax), "replace_original": false, "response_type": "in_channel"})
	if err != nil {
		return fmt.Errorf("slack channel %q reply: %w", ch.Name, err)
	}
	return RedactError(postSlackWebhook(ctx, s.client, a.ResponseURL, payload))
}
