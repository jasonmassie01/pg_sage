package store

import (
	"errors"
	"strings"
	"testing"
)

func TestSMTPDestinationSecretsRule(t *testing.T) {
	stored := map[string]string{
		"smtp_host": "smtp.example.com", "smtp_port": "587", "smtp_pass": "s3cret",
		"smtp_password": "legacy-s3cret",
	}
	cases := []struct {
		name     string
		incoming map[string]string
		refuse   bool
	}{
		{"host moves, masked pass", map[string]string{
			"smtp_host": "evil.example", "smtp_port": "587", "smtp_pass": "****"}, true},
		{"port moves, masked pass", map[string]string{
			"smtp_host": "smtp.example.com", "smtp_port": "25", "smtp_pass": "****"}, true},
		{"host moves, masked smtp_password", map[string]string{
			"smtp_host": "evil.example", "smtp_password": "****"}, true},
		{"default port equals 587", map[string]string{
			"smtp_host": "smtp.example.com", "smtp_pass": "****"}, false},
		{"host case and dot only", map[string]string{
			"smtp_host": "SMTP.example.com.", "smtp_port": "587", "smtp_pass": "****"}, false},
		{"host moves, new pass", map[string]string{
			"smtp_host": "evil.example", "smtp_pass": "typed"}, false},
		{"host moves, pass cleared", map[string]string{
			"smtp_host": "evil.example", "smtp_pass": ""}, false},
		{"host moves, pass omitted", map[string]string{"smtp_host": "evil.example"}, false},
		{"nil incoming", nil, false},
	}
	for _, c := range cases {
		err := checkSMTPDestinationSecrets(stored, c.incoming)
		if c.refuse != (err != nil) {
			t.Errorf("%s: err = %v, refuse = %v", c.name, err, c.refuse)
			continue
		}
		if err != nil && (!errors.Is(err, ErrValidation) ||
			!strings.Contains(err.Error(), "re-enter")) {
			t.Errorf("%s: error %v lacks ErrValidation or guidance", c.name, err)
		}
	}
}

func TestSMTPDestinationSecretsNothingStored(t *testing.T) {
	incoming := map[string]string{"smtp_host": "evil.example", "smtp_pass": "****"}
	if err := checkSMTPDestinationSecrets(nil, incoming); err != nil {
		t.Fatalf("nil stored = %v, want nil", err)
	}
	stored := map[string]string{"smtp_host": "smtp.example.com"}
	if err := checkSMTPDestinationSecrets(stored, incoming); err != nil {
		t.Fatalf("no stored password = %v, want nil", err)
	}
}
