package advisor

import (
	"testing"
)

func noopLog(string, string, ...any) {}

func TestStripToJSON_ValidArray(t *testing.T) {
	input := `[{"foo":"bar"}]`
	got := stripToJSON(input)
	if got != input {
		t.Fatalf("expected %q, got %q", input, got)
	}
}

func TestStripToJSON_WithThinkingPrefix(t *testing.T) {
	input := "Let me analyze...\n\n[{\"foo\":\"bar\"}]"
	want := `[{"foo":"bar"}]`
	got := stripToJSON(input)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripToJSON_WithMarkdownFences(t *testing.T) {
	input := "```json\n[{\"foo\":\"bar\"}]\n```"
	want := `[{"foo":"bar"}]`
	got := stripToJSON(input)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripToJSON_NoJSON(t *testing.T) {
	input := "No JSON here"
	got := stripToJSON(input)
	if got != input {
		t.Fatalf("expected original string %q, got %q", input, got)
	}
}

func TestStripToJSON_EmptyString(t *testing.T) {
	got := stripToJSON("")
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestStripToJSON_NestedBrackets(t *testing.T) {
	input := `text [{"a":[1,2]}] more`
	want := `[{"a":[1,2]}]`
	got := stripToJSON(input)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestParseLLMFindings_ValidJSON(t *testing.T) {
	raw := `[{` +
		`"object_identifier":"public.orders",` +
		`"severity":"info",` +
		`"rationale":"test",` +
		`"recommended_sql":"ALTER TABLE ..."}]`
	findings := parseLLMFindings(raw, "index_tuning", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "index_tuning" {
		t.Fatalf("expected category 'index_tuning', got %q", f.Category)
	}
	if f.ObjectIdentifier != "public.orders" {
		t.Fatalf(
			"expected ObjectIdentifier 'public.orders', got %q",
			f.ObjectIdentifier,
		)
	}
	if f.Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", f.Severity)
	}
	if f.Recommendation != "test" {
		t.Fatalf("expected recommendation 'test', got %q", f.Recommendation)
	}
	if f.RecommendedSQL != "ALTER TABLE ..." {
		t.Fatalf(
			"expected RecommendedSQL 'ALTER TABLE ...', got %q",
			f.RecommendedSQL,
		)
	}
}

func TestParseLLMFindings_EmptyArray(t *testing.T) {
	findings := parseLLMFindings("[]", "test", noopLog)
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(findings))
	}
}

func TestParseLLMFindings_InvalidJSON(t *testing.T) {
	findings := parseLLMFindings("not json", "test", noopLog)
	if findings != nil {
		t.Fatalf("expected nil findings, got %v", findings)
	}
}

func TestParseLLMFindings_ObjectIdentifierFallback(t *testing.T) {
	raw := `[{"table":"orders"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "orders" {
		t.Fatalf(
			"expected ObjectIdentifier 'orders', got %q",
			findings[0].ObjectIdentifier,
		)
	}
}

func TestParseLLMFindings_DefaultSeverity(t *testing.T) {
	raw := `[{"table":"orders"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Fatalf(
			"expected default severity 'info', got %q",
			findings[0].Severity,
		)
	}
}

func TestParseLLMFindings_DefaultObjectIdentifier(t *testing.T) {
	raw := `[{"rationale":"test"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "instance" {
		t.Fatalf(
			"expected default ObjectIdentifier 'instance', got %q",
			findings[0].ObjectIdentifier,
		)
	}
}

func TestParseLLMFindings_MultipleFindings(t *testing.T) {
	raw := `[{"table":"t1"},{"table":"t2"},{"table":"t3"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(findings))
	}
}

func TestParseLLMFindings_WithThinkingPrefix(t *testing.T) {
	raw := "I think...\n[{\"table\":\"t1\"}]"
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
}

func TestParseLLMFindings_CategorySet(t *testing.T) {
	raw := `[{"table":"t1"},{"table":"t2"}]`
	findings := parseLLMFindings(raw, "vacuum_tuning", noopLog)
	for i, f := range findings {
		if f.Category != "vacuum_tuning" {
			t.Fatalf(
				"finding[%d]: expected category 'vacuum_tuning', got %q",
				i, f.Category,
			)
		}
	}
}

func TestParseLLMFindings_ActionRiskDefault(t *testing.T) {
	raw := `[{"table":"t1"},{"table":"t2"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	for i, f := range findings {
		if f.ActionRisk != "safe" {
			t.Fatalf(
				"finding[%d]: expected ActionRisk 'safe', got %q",
				i, f.ActionRisk,
			)
		}
	}
}
