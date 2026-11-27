package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-06 M4: webhook is a notification channel type. Its URL goes
// through the target policy and its headers must be a JSON object of
// valid header names, refused at write time rather than at send time.
// No concurrent access tests: pure validation functions.

func TestValidateChannelType_WebhookIsValid(t *testing.T) {
	if err := validateChannelType("webhook"); err != nil {
		t.Fatalf("validateChannelType(webhook) = %v, want nil", err)
	}
	err := validateChannelType("sms")
	if err == nil || !strings.Contains(err.Error(), "telegram or webhook") {
		t.Fatalf("error = %v, want it to list webhook", err)
	}
}

func TestValidateChannelConfig_Webhook(t *testing.T) {
	ok := []map[string]string{
		{"webhook_url": "https://hooks.example.com/x"},
		{"webhook_url": "https://hooks.example.com/x", "headers": `{"X-Token":"t"}`},
		{"webhook_url": "https://hooks.example.com/x", "headers": `{}`},
	}
	for _, cfg := range ok {
		if err := validateChannelConfig("webhook", cfg); err != nil {
			t.Errorf("validateChannelConfig(%v) = %v, want nil", cfg, err)
		}
	}
	bad := map[string]map[string]string{
		"missing url":   {},
		"empty url":     {"webhook_url": ""},
		"headers text":  {"webhook_url": "https://h.example.com", "headers": "x"},
		"headers array": {"webhook_url": "https://h.example.com", "headers": `["a"]`},
		"header number": {"webhook_url": "https://h.example.com", "headers": `{"A":1}`},
		"header name":   {"webhook_url": "https://h.example.com", "headers": `{"A B":"v"}`},
		"host header":   {"webhook_url": "https://h.example.com", "headers": `{"host":"v"}`},
	}
	for name, cfg := range bad {
		err := validateChannelConfig("webhook", cfg)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

func TestValidateChannelTargets_WebhookUsesPolicy(t *testing.T) {
	cfg := map[string]string{"webhook_url": "http://127.0.0.1:9/hook"}
	if err := validateChannelTargets(notify.TargetPolicy{}, "webhook", cfg); !errors.Is(err,
		ErrValidation) {
		t.Fatalf("strict policy accepted a loopback webhook: %v", err)
	}
	if err := validateChannelTargets(notify.TargetPolicy{AllowPrivate: true}, "webhook",
		cfg); err != nil {
		t.Fatalf("AllowPrivate refused a loopback webhook: %v", err)
	}
	meta := map[string]string{"webhook_url": "http://169.254.169.254/latest"}
	if err := validateChannelTargets(notify.TargetPolicy{AllowPrivate: true}, "webhook",
		meta); err == nil {
		t.Fatal("the metadata endpoint passed the webhook target check")
	}
}
