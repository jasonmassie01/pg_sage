package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/notify"
)

var validChannelTypes = map[string]bool{
	"slack":     true,
	"email":     true,
	"pagerduty": true,
}

var notificationSecretKeys = []string{
	"webhook_url", "routing_key", "smtp_pass", "smtp_password",
	"api_key", "token", "secret",
}

func validateChannelType(typ string) error {
	if !validChannelTypes[typ] {
		return fmt.Errorf(
			"%w: type must be slack, email, or "+
				"pagerduty, got %q", ErrValidation, typ)
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
				"%w: slack channel requires webhook_url",
				ErrValidation)
		}
	case "email":
		if config["smtp_host"] == "" {
			return fmt.Errorf(
				"%w: email channel requires smtp_host",
				ErrValidation)
		}
		if config["from"] == "" {
			return fmt.Errorf(
				"%w: email channel requires from",
				ErrValidation)
		}
		if config["to"] == "" {
			return fmt.Errorf(
				"%w: email channel requires to",
				ErrValidation)
		}
	case "pagerduty":
		if config["routing_key"] == "" {
			return fmt.Errorf(
				"%w: pagerduty channel requires routing_key",
				ErrValidation)
		}
	}
	return nil
}

func validateEventType(event string) error {
	if !notify.ValidEventTypes[event] {
		return fmt.Errorf(
			"%w: invalid event type %q", ErrValidation, event)
	}
	return nil
}

func validateSeverity(sev string) error {
	if _, ok := notify.ValidSeverities[sev]; !ok {
		return fmt.Errorf(
			"%w: severity must be info, warning, or "+
				"critical, got %q", ErrValidation, sev)
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
