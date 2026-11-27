package config

import "testing"

// llm-09: llm.redact_numeric_literals turns on the redaction of 6+ digit
// numbers in comparisons before text reaches a model. It defaults to true
// (privacy first) with or without a config file, and is hot-reloadable.

func TestRedactNumericLiteralsDefaultsTrue(t *testing.T) {
	if !DefaultConfig().LLM.RedactNumericLiterals {
		t.Fatal("DefaultConfig llm.redact_numeric_literals = false, want true")
	}
	if !DefaultLLMRedactNumericLiterals {
		t.Fatal("DefaultLLMRedactNumericLiterals = false, want true")
	}
	if !loadNoConfigFile(t).LLM.RedactNumericLiterals {
		t.Fatal("Load without a file: llm.redact_numeric_literals = false (default masking)")
	}
	partial := loadYAMLConfig(t, "llm:\n  model: gpt-6-luna\n")
	if !partial.LLM.RedactNumericLiterals {
		t.Fatal("partial llm section: llm.redact_numeric_literals = false (default masking)")
	}
}

func TestRedactNumericLiteralsLoadsFalseFromYAML(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  redact_numeric_literals: false\n")
	if cfg.LLM.RedactNumericLiterals {
		t.Fatal("redact_numeric_literals: false loaded as true")
	}
	cfg = loadYAMLConfig(t, "llm:\n  redact_numeric_literals: true\n")
	if !cfg.LLM.RedactNumericLiterals {
		t.Fatal("redact_numeric_literals: true loaded as false")
	}
}

func TestRedactNumericLiteralsInvalidYAML(t *testing.T) {
	chdirTemp(t)
	path := writeYAMLFile(t, "llm:\n  redact_numeric_literals: maybe\n")
	_, err := Load([]string{"--config", path, "--pg-url", "postgres://u@localhost/db"})
	if err == nil {
		t.Fatal("redact_numeric_literals: maybe loaded without error")
	}
}

func TestRedactNumericLiteralsLifecycleAndDocs(t *testing.T) {
	lc, ok := LookupFieldLifecycle("llm.redact_numeric_literals")
	if !ok || lc.Lifecycle != LifecycleReconfigure || lc.Owner != LLMRedactionOwner {
		t.Fatalf("lifecycle = %+v (%v), want reconfigure by %s", lc, ok, LLMRedactionOwner)
	}
	docs := collectDocTags(t, DefaultConfig())
	if n := len(docs["llm.redact_numeric_literals"]); n < 20 || n > 200 {
		t.Fatalf("doc length %d outside [20,200]", n)
	}
}
