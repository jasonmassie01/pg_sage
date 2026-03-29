package store

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

func TestValidateChannelType_Valid(t *testing.T) {
	for typ := range validChannelTypes {
		if err := validateChannelType(typ); err != nil {
			t.Errorf("validateChannelType(%q) = %v, want nil", typ, err)
		}
	}
}

func TestValidateChannelType_Invalid(t *testing.T) {
	invalid := []string{
		"", "webhook", "sms", "pagerduty", "SLACK", "Email",
		"slack ", " email",
	}
	for _, typ := range invalid {
		err := validateChannelType(typ)
		if err == nil {
			t.Errorf("validateChannelType(%q) = nil, want error", typ)
			continue
		}
		if !strings.Contains(err.Error(), "validate") {
			t.Errorf("validateChannelType(%q) error = %q, "+
				"want to contain 'validate'", typ, err.Error())
		}
	}
}

func TestValidateChannelConfig_SlackValid(t *testing.T) {
	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/services/T00/B00/xxx",
	}
	if err := validateChannelConfig("slack", cfg); err != nil {
		t.Errorf("valid slack config: %v", err)
	}
}

func TestValidateChannelConfig_SlackMissingWebhookURL(t *testing.T) {
	cfg := map[string]string{}
	err := validateChannelConfig("slack", cfg)
	if err == nil {
		t.Fatal("slack config without webhook_url: want error, got nil")
	}
	if !strings.Contains(err.Error(), "webhook_url") {
		t.Errorf("error = %q, want to mention webhook_url", err.Error())
	}
}

func TestValidateChannelConfig_SlackEmptyWebhookURL(t *testing.T) {
	cfg := map[string]string{"webhook_url": ""}
	err := validateChannelConfig("slack", cfg)
	if err == nil {
		t.Fatal("slack config with empty webhook_url: want error")
	}
}

func TestValidateChannelConfig_EmailValid(t *testing.T) {
	cfg := map[string]string{
		"smtp_host": "smtp.example.com",
		"from":      "noreply@example.com",
		"to":        "admin@example.com",
	}
	if err := validateChannelConfig("email", cfg); err != nil {
		t.Errorf("valid email config: %v", err)
	}
}

func TestValidateChannelConfig_EmailMissingSMTPHost(t *testing.T) {
	cfg := map[string]string{
		"from": "noreply@example.com",
		"to":   "admin@example.com",
	}
	err := validateChannelConfig("email", cfg)
	if err == nil {
		t.Fatal("email without smtp_host: want error, got nil")
	}
	if !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("error = %q, want to mention smtp_host", err.Error())
	}
}

func TestValidateChannelConfig_EmailMissingFrom(t *testing.T) {
	cfg := map[string]string{
		"smtp_host": "smtp.example.com",
		"to":        "admin@example.com",
	}
	err := validateChannelConfig("email", cfg)
	if err == nil {
		t.Fatal("email without from: want error, got nil")
	}
	if !strings.Contains(err.Error(), "from") {
		t.Errorf("error = %q, want to mention from", err.Error())
	}
}

func TestValidateChannelConfig_EmailMissingTo(t *testing.T) {
	cfg := map[string]string{
		"smtp_host": "smtp.example.com",
		"from":      "noreply@example.com",
	}
	err := validateChannelConfig("email", cfg)
	if err == nil {
		t.Fatal("email without to: want error, got nil")
	}
	if !strings.Contains(err.Error(), "to") {
		t.Errorf("error = %q, want to mention to", err.Error())
	}
}

func TestValidateChannelConfig_EmailAllMissing(t *testing.T) {
	cfg := map[string]string{}
	err := validateChannelConfig("email", cfg)
	if err == nil {
		t.Fatal("email with empty config: want error, got nil")
	}
	// Should fail on smtp_host first (order in switch).
	if !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("error = %q, want first failure on smtp_host",
			err.Error())
	}
}

func TestValidateChannelConfig_EmailEmptyValues(t *testing.T) {
	cfg := map[string]string{
		"smtp_host": "",
		"from":      "a@b.com",
		"to":        "c@d.com",
	}
	err := validateChannelConfig("email", cfg)
	if err == nil {
		t.Fatal("email with empty smtp_host: want error")
	}
}

func TestValidateChannelConfig_NilMap(t *testing.T) {
	// Slack with nil config should fail on missing webhook_url.
	err := validateChannelConfig("slack", nil)
	if err == nil {
		t.Fatal("slack with nil config: want error, got nil")
	}
}

func TestValidateEventType_AllValid(t *testing.T) {
	for evt := range notify.ValidEventTypes {
		if err := validateEventType(evt); err != nil {
			t.Errorf("validateEventType(%q) = %v, want nil", evt, err)
		}
	}
}

func TestValidateEventType_Invalid(t *testing.T) {
	invalid := []string{
		"", "unknown", "action", "finding",
		"ACTION_EXECUTED", "action-executed",
	}
	for _, evt := range invalid {
		err := validateEventType(evt)
		if err == nil {
			t.Errorf("validateEventType(%q) = nil, want error", evt)
			continue
		}
		if !strings.Contains(err.Error(), "invalid event type") {
			t.Errorf("validateEventType(%q) error = %q, "+
				"want 'invalid event type'", evt, err.Error())
		}
	}
}

func TestValidateSeverity_AllValid(t *testing.T) {
	for sev := range notify.ValidSeverities {
		if err := validateSeverity(sev); err != nil {
			t.Errorf("validateSeverity(%q) = %v, want nil", sev, err)
		}
	}
}

func TestValidateSeverity_Invalid(t *testing.T) {
	invalid := []string{
		"", "low", "high", "error", "INFO", "Warning",
		"CRITICAL", "fatal",
	}
	for _, sev := range invalid {
		err := validateSeverity(sev)
		if err == nil {
			t.Errorf("validateSeverity(%q) = nil, want error", sev)
			continue
		}
		if !strings.Contains(err.Error(), "severity must be") {
			t.Errorf("validateSeverity(%q) error = %q, "+
				"want 'severity must be'", sev, err.Error())
		}
	}
}

func TestParseJSONConfig_ValidJSON(t *testing.T) {
	data := []byte(`{"smtp_host":"mail.example.com","from":"a@b.com"}`)
	got := parseJSONConfig(data)

	if len(got) != 2 {
		t.Fatalf("parseJSONConfig returned %d entries, want 2", len(got))
	}
	if got["smtp_host"] != "mail.example.com" {
		t.Errorf("smtp_host = %q, want mail.example.com",
			got["smtp_host"])
	}
	if got["from"] != "a@b.com" {
		t.Errorf("from = %q, want a@b.com", got["from"])
	}
}

func TestParseJSONConfig_EmptyBytes(t *testing.T) {
	got := parseJSONConfig([]byte{})
	if got == nil {
		t.Fatal("parseJSONConfig(empty) = nil, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("parseJSONConfig(empty) has %d entries, want 0",
			len(got))
	}
}

func TestParseJSONConfig_NilBytes(t *testing.T) {
	got := parseJSONConfig(nil)
	if got == nil {
		t.Fatal("parseJSONConfig(nil) = nil, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("parseJSONConfig(nil) has %d entries, want 0",
			len(got))
	}
}

func TestParseJSONConfig_MalformedJSON(t *testing.T) {
	// Malformed JSON should return an empty map (error is swallowed).
	got := parseJSONConfig([]byte(`{not valid json`))
	if got == nil {
		t.Fatal("parseJSONConfig(malformed) = nil, want non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("parseJSONConfig(malformed) has %d entries, want 0",
			len(got))
	}
}

func TestParseJSONConfig_EmptyObject(t *testing.T) {
	got := parseJSONConfig([]byte(`{}`))
	if got == nil {
		t.Fatal("parseJSONConfig({}) = nil, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("parseJSONConfig({}) has %d entries, want 0",
			len(got))
	}
}

func TestParseJSONConfig_NonObjectJSON(t *testing.T) {
	// JSON array should fail to unmarshal into map[string]string.
	got := parseJSONConfig([]byte(`["a","b"]`))
	if got == nil {
		t.Fatal("parseJSONConfig(array) = nil, want non-nil map")
	}
	// Should be empty since unmarshal into map fails.
	if len(got) != 0 {
		t.Errorf("parseJSONConfig(array) has %d entries, want 0",
			len(got))
	}
}

func TestParseJSONConfig_EmptyStringValue(t *testing.T) {
	data := []byte(`{"key":""}`)
	got := parseJSONConfig(data)
	if got["key"] != "" {
		t.Errorf("key = %q, want empty string", got["key"])
	}
}

func TestValidateChannelConfig_EmailExtraFieldsAllowed(t *testing.T) {
	// Extra fields beyond required ones should not cause errors.
	cfg := map[string]string{
		"smtp_host": "smtp.example.com",
		"from":      "a@b.com",
		"to":        "c@d.com",
		"smtp_port": "587",
		"username":  "user",
		"password":  "pass",
	}
	if err := validateChannelConfig("email", cfg); err != nil {
		t.Errorf("email with extra fields: %v", err)
	}
}

func TestValidateChannelConfig_SlackExtraFieldsAllowed(t *testing.T) {
	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/x",
		"channel":     "#alerts",
	}
	if err := validateChannelConfig("slack", cfg); err != nil {
		t.Errorf("slack with extra fields: %v", err)
	}
}
