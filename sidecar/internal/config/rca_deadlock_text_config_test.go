package config

import (
	"strings"
	"testing"
)

// rca-07b: rca.deadlock_statement_text decides whether a deadlock
// participant keeps its redacted statement text or only its fingerprint.

// No concurrent access tests: a pure accessor and load-time validation of
// an immutable config value.

// Default-value masking guard: with no config file no statement text is kept.
func TestRCADeadlockStatementText_DefaultIsFingerprint(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.DeadlockStatementText != DeadlockStatementTextFingerprint ||
		DefaultRCADeadlockStatementText != "fingerprint" {
		t.Errorf("deadlock_statement_text = %q (default %q), want fingerprint",
			cfg.RCA.DeadlockStatementText, DefaultRCADeadlockStatementText)
	}
}

// Nil/zero: a config built without the key is valid; the classifier then
// keeps no text (TestClassifier_DeadlockParticipantsFingerprintMode).
func TestRCADeadlockStatementText_ZeroValueIsValid(t *testing.T) {
	var r RCAConfig
	if err := r.validateDeadlockStatementText(); err != nil {
		t.Errorf("empty value rejected: %v", err)
	}
}

// Happy path and state: redacted is accepted and switches the text on.
func TestRCADeadlockStatementText_YAMLRedacted(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  deadlock_statement_text: redacted\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.DeadlockStatementText != DeadlockStatementTextRedacted {
		t.Errorf("deadlock_statement_text = %q, want redacted", cfg.RCA.DeadlockStatementText)
	}
}

// Invalid input: anything else (including raw text or a different case)
// fails at load with the key and the allowed values in the message.
func TestRCADeadlockStatementText_RejectsUnknownValues(t *testing.T) {
	for _, v := range []string{"raw", "REDACTED", "full", "none"} {
		_, err := loadRCAYAML(t, "rca:\n  deadlock_statement_text: "+v+"\n")
		if err == nil {
			t.Errorf("%q accepted, want a load error", v)
			continue
		}
		msg := err.Error()
		for _, want := range []string{"rca.deadlock_statement_text", "fingerprint",
			"redacted", v} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q: error %q lacks %q", v, msg, want)
			}
		}
	}
}
