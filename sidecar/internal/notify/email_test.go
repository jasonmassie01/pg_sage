package notify

import (
	"testing"
)

func TestEmailSend_MissingHost(t *testing.T) {
	sender := NewEmailSender()
	ch := Channel{
		ID:     1,
		Name:   "no-host",
		Type:   "email",
		Config: map[string]string{"from": "a@b.com", "to": "c@d.com"},
	}
	evt := Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  "Test",
		Body:     "Body",
	}

	err := sender.Send(t.Context(), ch, evt)
	if err == nil {
		t.Fatal("expected error for missing smtp_host")
	}
}

func TestEmailSend_MissingFrom(t *testing.T) {
	sender := NewEmailSender()
	ch := Channel{
		ID:     1,
		Name:   "no-from",
		Type:   "email",
		Config: map[string]string{
			"smtp_host": "mail.example.com", "to": "c@d.com",
		},
	}
	evt := Event{
		Type:     "action_failed",
		Severity: "warning",
		Subject:  "Test",
	}

	err := sender.Send(t.Context(), ch, evt)
	if err == nil {
		t.Fatal("expected error for missing from")
	}
}

func TestEmailSend_MissingTo(t *testing.T) {
	sender := NewEmailSender()
	ch := Channel{
		ID:     1,
		Name:   "no-to",
		Type:   "email",
		Config: map[string]string{
			"smtp_host": "mail.example.com",
			"from":      "a@b.com",
		},
	}
	evt := Event{
		Type:     "finding_critical",
		Severity: "critical",
		Subject:  "Test",
	}

	err := sender.Send(t.Context(), ch, evt)
	if err == nil {
		t.Fatal("expected error for missing to")
	}
}

func TestFormatEmailMessage(t *testing.T) {
	cfg := &emailConfig{
		Host: "smtp.example.com",
		Port: "587",
		From: "sage@example.com",
		To:   []string{"admin@example.com", "ops@example.com"},
	}
	evt := Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  "Test Subject",
		Body:     "Test body content",
	}

	msg := formatEmailMessage(cfg, evt)

	checks := []string{
		"From: sage@example.com",
		"To: admin@example.com, ops@example.com",
		"Subject: [pg_sage] Test Subject",
		"Test body content",
		"Severity: info",
	}
	for _, want := range checks {
		found := false
		for i := range len(msg) - len(want) + 1 {
			if msg[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("message missing %q", want)
		}
	}
}

func TestSplitRecipients(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"a@b.com", 1},
		{"a@b.com, c@d.com", 2},
		{"a@b.com,c@d.com,e@f.com", 3},
		{" a@b.com , , c@d.com ", 2},
	}
	for _, tt := range tests {
		got := splitRecipients(tt.input)
		if len(got) != tt.want {
			t.Errorf(
				"splitRecipients(%q) = %d items, want %d",
				tt.input, len(got), tt.want)
		}
	}
}
