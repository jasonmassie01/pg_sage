package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/notify"
)

var validChannelTypes = map[string]bool{
	"slack": true,
	"email": true,
}

func validateChannelType(typ string) error {
	if !validChannelTypes[typ] {
		return fmt.Errorf(
			"validate: type must be slack or email, got %q", typ)
	}
	return nil
}

func validateChannelConfig(
	typ string, config map[string]string,
) error {
	switch typ {
	case "slack":
		if config["webhook_url"] == "" {
			return fmt.Errorf(
				"validate: slack channel requires webhook_url")
		}
	case "email":
		if config["smtp_host"] == "" {
			return fmt.Errorf(
				"validate: email channel requires smtp_host")
		}
		if config["from"] == "" {
			return fmt.Errorf(
				"validate: email channel requires from")
		}
		if config["to"] == "" {
			return fmt.Errorf(
				"validate: email channel requires to")
		}
	}
	return nil
}

func validateEventType(event string) error {
	if !notify.ValidEventTypes[event] {
		return fmt.Errorf(
			"validate: invalid event type %q", event)
	}
	return nil
}

func validateSeverity(sev string) error {
	if _, ok := notify.ValidSeverities[sev]; !ok {
		return fmt.Errorf(
			"validate: severity must be info, warning, or "+
				"critical, got %q", sev)
	}
	return nil
}

func parseJSONConfig(data []byte) map[string]string {
	m := make(map[string]string)
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

// sendTestDirect sends a test event through a specific channel
// using the dispatcher's registered senders.
func sendTestDirect(
	ctx context.Context,
	d *notify.Dispatcher,
	ch notify.Channel,
	evt notify.Event,
) error {
	// Use Dispatch which routes through all matching rules,
	// but for test we want direct send. Use the dispatcher's
	// public Dispatch with a synthetic approach.
	return d.SendDirect(ctx, ch, evt)
}
