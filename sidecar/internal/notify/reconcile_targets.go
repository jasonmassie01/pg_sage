package notify

import (
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
)

// targetChannels maps each usable YAML target to the channel it becomes,
// keyed like config.AlertingConfig.ResolveAlertChannel: "slack",
// "pagerduty" and "webhook:<name>". A target the policy refuses builds no
// channel and is logged by its key, as the legacy engine did (alerting-11).
func targetChannels(
	a config.AlertingConfig, policy TargetPolicy, logFn func(string, string, ...any),
) map[string]ManagedChannel {
	out := map[string]ManagedChannel{}
	if u := a.SlackWebhookURL; u != "" {
		if err := policy.ValidateURL(u); err != nil {
			logRefusedTarget(logFn, "alerting.slack_webhook_url", err)
		} else {
			out["slack"] = managedChannel("slack", "slack",
				map[string]string{"webhook_url": u})
		}
	}
	if k := a.PagerDutyRoutingKey; k != "" {
		out["pagerduty"] = managedChannel("pagerduty", "pagerduty",
			map[string]string{"routing_key": k})
	}
	for i, wh := range a.Webhooks {
		if wh.Name == "" || wh.URL == "" {
			continue // refused by config validation
		}
		key := fmt.Sprintf("alerting.webhooks[%d].url (webhook %q)", i, wh.Name)
		if err := policy.ValidateURL(wh.URL); err != nil {
			logRefusedTarget(logFn, key, err)
			continue
		}
		cfg := map[string]string{"webhook_url": wh.URL}
		if len(wh.Headers) > 0 {
			hdr, err := json.Marshal(wh.Headers)
			if err != nil {
				logRefusedTarget(logFn, key, err)
				continue
			}
			cfg["headers"] = string(hdr)
		}
		out["webhook:"+wh.Name] = managedChannel("webhook", wh.Name, cfg)
	}
	return out
}

func managedChannel(typ, name string, cfg map[string]string) ManagedChannel {
	return ManagedChannel{Name: ConfigChannelName(typ, name), Type: typ, Config: cfg,
		Hash: channelHash(typ, cfg)}
}

// logRefusedTarget names the refused key; the error never carries the
// URL's secret path, only the policy's reason.
func logRefusedTarget(logFn func(string, string, ...any), key string, err error) {
	logFn("WARN", "alerting: %s is refused by the notification target policy; no "+
		"channel is built for it: %v", key, RedactError(err))
}
