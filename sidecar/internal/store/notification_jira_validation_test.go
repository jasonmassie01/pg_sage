package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// market-14: a Jira integration is stored like a channel (sealed
// api_token, target policy on base_url) but is a case link, not an alert
// destination: no rule may route events to it.
// No concurrent access tests: pure validation functions.

func jiraConfig() map[string]string {
	return map[string]string{"base_url": "https://acme.atlassian.net",
		"email": "bot@example.com", "api_token": "t", "project_key": "OPS"}
}

func TestValidateChannelConfig_Jira(t *testing.T) {
	if err := validateChannelType("jira"); err != nil {
		t.Fatalf("jira type: %v", err)
	}
	if err := validateChannelConfig("jira", jiraConfig()); err != nil {
		t.Fatalf("valid jira config: %v", err)
	}
	withType := jiraConfig()
	withType["issue_type"] = "Bug"
	if err := validateChannelConfig("jira", withType); err != nil {
		t.Fatalf("jira config with issue_type: %v", err)
	}
	bad := map[string]func(map[string]string){
		"no base_url":     func(c map[string]string) { delete(c, "base_url") },
		"http base_url":   func(c map[string]string) { c["base_url"] = "http://a.example.net" },
		"base_url path":   func(c map[string]string) { c["base_url"] = "https://a.net/x?y=1" },
		"no email":        func(c map[string]string) { delete(c, "email") },
		"no api_token":    func(c map[string]string) { delete(c, "api_token") },
		"no project_key":  func(c map[string]string) { delete(c, "project_key") },
		"lowercase key":   func(c map[string]string) { c["project_key"] = "ops" },
		"key with dash":   func(c map[string]string) { c["project_key"] = "OP-S" },
		"long issue type": func(c map[string]string) { c["issue_type"] = strings.Repeat("x", 61) },
	}
	for name, mutate := range bad {
		cfg := jiraConfig()
		mutate(cfg)
		if err := validateChannelConfig("jira", cfg); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
	if err := validateChannelConfig("jira", nil); !errors.Is(err, ErrValidation) {
		t.Errorf("nil config: err = %v, want ErrValidation", err)
	}
}

func TestValidateChannelTargets_JiraUsesPolicy(t *testing.T) {
	cfg := jiraConfig()
	cfg["base_url"] = "https://192.168.1.10"
	if err := validateChannelTargets(notify.TargetPolicy{}, "jira", cfg); !errors.Is(err,
		ErrValidation) {
		t.Fatalf("strict policy accepted a private Jira URL: %v", err)
	}
	if err := validateChannelTargets(notify.TargetPolicy{AllowPrivate: true}, "jira",
		cfg); err != nil {
		t.Fatalf("allow_private_targets refused a private Jira URL: %v", err)
	}
}

func TestValidateRuleChannel_RefusesTicketIntegrations(t *testing.T) {
	err := validateRuleChannel(&notify.Channel{ID: 3, Type: "jira"})
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "ticket") {
		t.Fatalf("jira rule err = %v, want ErrValidation naming tickets", err)
	}
	for _, typ := range []string{"slack", "teams", "opsgenie", "incidentio", "webhook"} {
		if err := validateRuleChannel(&notify.Channel{Type: typ}); err != nil {
			t.Errorf("%s rule refused: %v", typ, err)
		}
	}
}

func TestNotificationSecretKeys_SealJiraToken(t *testing.T) {
	for _, k := range notificationSecretKeys {
		if k == "api_token" {
			return
		}
	}
	t.Fatal("api_token is not sealed at rest")
}
