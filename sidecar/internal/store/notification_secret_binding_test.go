package store

import (
	"errors"
	"strings"
	"testing"
)

// A masked secret kept on update goes wherever the channel now points, so
// moving a Jira integration or a webhook to a new target while sending the
// masked api_token or headers would hand them to that target (market-14).

func TestDestinationSecretsRule_Jira(t *testing.T) {
	stored := map[string]string{"base_url": "https://acme.atlassian.net",
		"email": "ops@acme.test", "api_token": "real-token", "project_key": "OPS"}
	cases := []struct {
		name     string
		incoming map[string]string
		refuse   bool
	}{
		{"site moves, masked token", map[string]string{
			"base_url": "https://evil.example", "api_token": "real****oken"}, true},
		{"site moves, fully masked token", map[string]string{
			"base_url": "https://evil.example", "api_token": "****"}, true},
		{"site cleared, masked token", map[string]string{
			"base_url": "", "api_token": "****"}, true},
		{"same site, masked token", map[string]string{
			"base_url": "https://acme.atlassian.net", "api_token": "****"}, false},
		{"same site, case and trailing slash", map[string]string{
			"base_url": " https://ACME.atlassian.net/ ", "api_token": "****"}, false},
		{"site moves, new token", map[string]string{
			"base_url": "https://evil.example", "api_token": "typed"}, false},
		{"site moves, token cleared", map[string]string{
			"base_url": "https://evil.example", "api_token": ""}, false},
		{"site moves, token omitted", map[string]string{
			"base_url": "https://evil.example"}, false},
		{"nil incoming", nil, false},
	}
	for _, c := range cases {
		err := checkDestinationSecrets("jira", stored, c.incoming)
		if c.refuse != (err != nil) {
			t.Errorf("%s: err = %v, refuse = %v", c.name, err, c.refuse)
			continue
		}
		if err != nil && (!errors.Is(err, ErrValidation) ||
			!strings.Contains(err.Error(), "re-enter api_token")) {
			t.Errorf("%s: error %v lacks ErrValidation or guidance", c.name, err)
		}
	}
}

func TestDestinationSecretsRule_WebhookHeaders(t *testing.T) {
	stored := map[string]string{"webhook_url": "https://hooks.example.com/a",
		"headers": `{"Authorization":"Bearer real"}`}
	cases := []struct {
		name     string
		incoming map[string]string
		refuse   bool
	}{
		{"url moves, masked headers", map[string]string{
			"webhook_url": "https://evil.example/x", "headers": `{"Au****al"}`}, true},
		{"url masked (unchanged), masked headers", map[string]string{
			"webhook_url": "http****om/a", "headers": "****"}, false},
		{"same url, masked headers", map[string]string{
			"webhook_url": "https://hooks.example.com/a", "headers": "****"}, false},
		{"url moves, new headers", map[string]string{
			"webhook_url": "https://evil.example/x", "headers": `{"X":"y"}`}, false},
		{"url moves, headers cleared", map[string]string{
			"webhook_url": "https://evil.example/x", "headers": ""}, false},
	}
	for _, c := range cases {
		err := checkDestinationSecrets("webhook", stored, c.incoming)
		if c.refuse != (err != nil) {
			t.Errorf("%s: err = %v, refuse = %v", c.name, err, c.refuse)
			continue
		}
		if err != nil && (!errors.Is(err, ErrValidation) ||
			!strings.Contains(err.Error(), "re-enter headers")) {
			t.Errorf("%s: error %v lacks ErrValidation or guidance", c.name, err)
		}
	}
}

func TestDestinationSecretsRule_NothingStoredOrOtherTypes(t *testing.T) {
	incoming := map[string]string{"base_url": "https://evil.example", "api_token": "****"}
	if err := checkDestinationSecrets("jira", nil, incoming); err != nil {
		t.Fatalf("nil stored = %v, want nil", err)
	}
	noToken := map[string]string{"base_url": "https://acme.atlassian.net"}
	if err := checkDestinationSecrets("jira", noToken, incoming); err != nil {
		t.Fatalf("no stored token = %v, want nil", err)
	}
	// Opsgenie and incident.io post only to their fixed APIs; Teams' only
	// secret is its URL, so nothing else can follow a moved target.
	og := map[string]string{"api_key": "k", "region": "us"}
	if err := checkDestinationSecrets("opsgenie", og,
		map[string]string{"api_key": "****", "region": "eu"}); err != nil {
		t.Fatalf("opsgenie region change = %v, want nil", err)
	}
	if err := checkDestinationSecrets("", og, og); err != nil {
		t.Fatalf("empty type = %v, want nil", err)
	}
}
