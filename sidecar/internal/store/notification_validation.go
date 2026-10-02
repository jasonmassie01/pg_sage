package store

import (
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/notify"
)

var validChannelTypes = map[string]bool{
	"slack":     true,
	"email":     true,
	"pagerduty": true,
	"telegram":  true,
}

var notificationSecretKeys = notify.SecretConfigKeys

func validateChannelType(typ string) error {
	if !validChannelTypes[typ] {
		return fmt.Errorf(
			"%w: type must be slack, email, "+
				"pagerduty or telegram, got %q", ErrValidation, typ)
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
		return validateSlackInteractive(config)
	case "telegram":
		return validateTelegramConfig(config)
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

// validateChannelTargets rejects webhook URLs and SMTP hosts that point
// at internal or metadata addresses, or use a non-https scheme (G7-B21).
// The senders re-check resolved addresses at dial time.
func validateChannelTargets(
	policy notify.TargetPolicy, typ string, config map[string]string,
) error {
	var err error
	switch typ {
	case "slack":
		err = policy.ValidateURL(config["webhook_url"])
	case "email":
		err = policy.ValidateHost(config["smtp_host"])
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}
