package store

import (
	"fmt"
	"net/url"
	"regexp"
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
	case "jira":
		return validateJiraConfig(config)
	}
	return nil
}

var jiraProjectKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,19}$`)

// validateJiraConfig checks a Jira Cloud ticket integration: an https
// site URL with no path or query, the account email and API token, a
// project key and an optional issue type (default Task).
func validateJiraConfig(config map[string]string) error {
	u, err := url.Parse(config["base_url"])
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.Trim(u.Path, "/") != "" ||
		u.RawQuery != "" {
		return fmt.Errorf("%w: jira base_url must be an https site URL such as "+
			"https://acme.atlassian.net", ErrValidation)
	}
	for _, key := range []string{"email", "api_token"} {
		if config[key] == "" {
			return fmt.Errorf("%w: jira integration requires %s", ErrValidation, key)
		}
	}
	if !jiraProjectKey.MatchString(config["project_key"]) {
		return fmt.Errorf("%w: jira project_key must be 2-20 capital letters, digits "+
			"or '_', starting with a letter", ErrValidation)
	}
	if len(config["issue_type"]) > 60 {
		return fmt.Errorf("%w: jira issue_type is at most 60 characters", ErrValidation)
	}
	return nil
}

// validateRuleChannel refuses a rule on a ticket integration: Jira links
// cases, it does not receive events (market-14).
func validateRuleChannel(ch *notify.Channel) error {
	if notify.IsTicketIntegration(ch.Type) {
		return fmt.Errorf("%w: channel %d is a %s ticket integration; tickets are created "+
			"from a case, not by notification rules", ErrValidation, ch.ID, ch.Type)
	}
	return nil
}

// validateDestinationTarget checks the URL a Teams channel posts to and
// a Jira integration's site against the target policy; Opsgenie and
// incident.io post only to their fixed public APIs.
func validateDestinationTarget(
	policy notify.TargetPolicy, typ string, config map[string]string,
) error {
	switch typ {
	case "teams":
		return policy.ValidateURL(config["webhook_url"])
	case "jira":
		return policy.ValidateURL(config["base_url"])
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
