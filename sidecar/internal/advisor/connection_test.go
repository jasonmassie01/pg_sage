package advisor

import (
	"strings"
	"testing"
)

// Integration tests cover the full analyzeConnections flow (requires LLM + DB).
// These unit tests validate prompt content and structural invariants.

func TestConnectionSystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"max_connections",
		"PgBouncer",
		"peak_active",
		"idle_in_transaction",
	}
	for _, want := range checks {
		if !strings.Contains(connectionSystemPrompt, want) {
			t.Errorf("connectionSystemPrompt missing expected text %q", want)
		}
	}
}

func TestConnectionSystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(connectionSystemPrompt, "No thinking") {
		t.Error("connectionSystemPrompt missing 'No thinking' anti-thinking directive")
	}
}

func TestConnectionSystemPrompt_JSONFormat(t *testing.T) {
	if !strings.Contains(connectionSystemPrompt, "JSON array") {
		t.Error("connectionSystemPrompt missing 'JSON array' format requirement")
	}
	if !strings.Contains(connectionSystemPrompt, "object_identifier") {
		t.Error("connectionSystemPrompt missing 'object_identifier' field in schema")
	}
}
