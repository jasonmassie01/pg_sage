package store

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/pg-sage/sidecar/internal/notify"
)

// validateDestinationConfig checks the Teams, Opsgenie and incident.io
// channel settings (market-14), so a channel that cannot deliver is
// refused when it is saved.
func validateDestinationConfig(typ string, config map[string]string) error {
	switch typ {
	case "teams":
		return validateTeamsConfig(config)
	case "opsgenie":
		if config["api_key"] == "" {
			return fmt.Errorf("%w: opsgenie channel requires api_key", ErrValidation)
		}
		if _, err := notify.OpsgenieBaseURL(config["region"]); err != nil {
			return fmt.Errorf("%w: %v", ErrValidation, err)
		}
	case "incidentio":
		if config["token"] == "" {
			return fmt.Errorf("%w: incidentio channel requires token", ErrValidation)
		}
		if !notify.ValidIncidentIOSourceID(config["alert_source_config_id"]) {
			return fmt.Errorf("%w: incidentio channel requires alert_source_config_id "+
				"(1-64 letters, digits, '_' or '-')", ErrValidation)
		}
	}
	return nil
}

// validateTeamsConfig needs an https Workflows webhook URL (Teams issues
// no other kind) and, when set, an absolute http(s) dashboard_url for
// the card's link.
func validateTeamsConfig(config map[string]string) error {
	raw := strings.TrimSpace(config["webhook_url"])
	if raw == "" {
		return fmt.Errorf("%w: teams channel requires webhook_url", ErrValidation)
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: teams webhook_url must be an https URL", ErrValidation)
	}
	if link := config["dashboard_url"]; link != "" {
		u, err := url.Parse(link)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%w: teams dashboard_url must be an absolute http(s) URL",
				ErrValidation)
		}
	}
	return nil
}
