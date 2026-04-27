package migration

import (
	"testing"
)

func TestParseDDLLLMResponse_ValidJSON(t *testing.T) {
	raw := `{
		"lock_level": "ACCESS EXCLUSIVE",
		"requires_rewrite": true,
		"risk_score": 0.85,
		"safe_alternative": "ALTER TABLE foo ALTER COLUMN bar TYPE bigint;",
		"explanation": "Changing column type requires full table rewrite",
		"estimated_duration_seconds": 120
	}`

	resp, err := parseDDLLLMResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.LockLevel != "ACCESS EXCLUSIVE" {
		t.Errorf("lock_level = %q, want ACCESS EXCLUSIVE", resp.LockLevel)
	}
	if !resp.RequiresRewrite {
		t.Error("requires_rewrite = false, want true")
	}
	if resp.RiskScore != 0.85 {
		t.Errorf("risk_score = %f, want 0.85", resp.RiskScore)
	}
	if resp.SafeAlternative == "" {
		t.Error("safe_alternative is empty, want non-empty")
	}
	if resp.Explanation == "" {
		t.Error("explanation is empty, want non-empty")
	}
	if resp.EstDurationSec != 120 {
		t.Errorf("estimated_duration_seconds = %d, want 120", resp.EstDurationSec)
	}
}

func TestParseDDLLLMResponse_MarkdownWrapped(t *testing.T) {
	raw := "```json\n" +
		`{"lock_level":"SHARE","requires_rewrite":false,` +
		`"risk_score":0.4,"safe_alternative":"",` +
		`"explanation":"Creates a SHARE lock","estimated_duration_seconds":5}` +
		"\n```"

	resp, err := parseDDLLLMResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.LockLevel != "SHARE" {
		t.Errorf("lock_level = %q, want SHARE", resp.LockLevel)
	}
	if resp.RiskScore != 0.4 {
		t.Errorf("risk_score = %f, want 0.4", resp.RiskScore)
	}
}

func TestParseDDLLLMResponse_MarkdownWrappedNoLanguage(t *testing.T) {
	raw := "```\n" +
		`{"lock_level":"SHARE","requires_rewrite":false,` +
		`"risk_score":0.2,"safe_alternative":"",` +
		`"explanation":"Low risk DDL","estimated_duration_seconds":1}` +
		"\n```"

	resp, err := parseDDLLLMResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RiskScore != 0.2 {
		t.Errorf("risk_score = %f, want 0.2", resp.RiskScore)
	}
}

func TestParseDDLLLMResponse_WithPreambleText(t *testing.T) {
	raw := "Here is the analysis:\n" +
		`{"lock_level":"ACCESS EXCLUSIVE","requires_rewrite":true,` +
		`"risk_score":0.9,"safe_alternative":"use pg_repack",` +
		`"explanation":"Full rewrite needed","estimated_duration_seconds":300}` +
		"\nHope this helps!"

	resp, err := parseDDLLLMResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RiskScore != 0.9 {
		t.Errorf("risk_score = %f, want 0.9", resp.RiskScore)
	}
	if resp.Explanation != "Full rewrite needed" {
		t.Errorf("explanation = %q, want 'Full rewrite needed'",
			resp.Explanation)
	}
}

func TestParseDDLLLMResponse_EmptyExplanation(t *testing.T) {
	raw := `{"lock_level":"SHARE","requires_rewrite":false,` +
		`"risk_score":0.1,"safe_alternative":"",` +
		`"explanation":"","estimated_duration_seconds":1}`

	_, err := parseDDLLLMResponse(raw)
	if err == nil {
		t.Fatal("expected error for empty explanation, got nil")
	}
}

func TestParseDDLLLMResponse_InvalidJSON(t *testing.T) {
	_, err := parseDDLLLMResponse("this is not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseDDLLLMResponse_EmptyInput(t *testing.T) {
	_, err := parseDDLLLMResponse("")
	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

func TestStripToJSONObject_Plain(t *testing.T) {
	input := `{"key": "value"}`
	got := stripToJSONObject(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestStripToJSONObject_MarkdownFences(t *testing.T) {
	input := "```json\n{\"key\": \"value\"}\n```"
	want := `{"key": "value"}`
	got := stripToJSONObject(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToJSONObject_Preamble(t *testing.T) {
	input := "Here is the result:\n{\"key\": \"value\"}\nDone."
	want := `{"key": "value"}`
	got := stripToJSONObject(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripToJSONObject_NoJSON(t *testing.T) {
	input := "no json here"
	got := stripToJSONObject(input)
	if got != input {
		t.Errorf("got %q, want %q (should return input unchanged)",
			got, input)
	}
}

func TestStripToJSONObject_NestedBraces(t *testing.T) {
	input := `{"outer": {"inner": "val"}}`
	got := stripToJSONObject(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestBuildDDLSystemPrompt_NonEmpty(t *testing.T) {
	prompt := buildDDLSystemPrompt()
	if prompt == "" {
		t.Fatal("system prompt is empty")
	}
	if len(prompt) < 50 {
		t.Errorf("system prompt suspiciously short: %d chars", len(prompt))
	}
}

func TestBuildDDLUserPrompt_ContainsAllFields(t *testing.T) {
	sql := "ALTER TABLE foo ADD COLUMN bar int"
	prompt := buildDDLUserPrompt(sql, 160000, "mydb")

	if !contains(prompt, "160000") {
		t.Error("user prompt missing PG version")
	}
	if !contains(prompt, "mydb") {
		t.Error("user prompt missing database name")
	}
	if !contains(prompt, sql) {
		t.Error("user prompt missing SQL statement")
	}
}

func TestBuildDDLUserPrompt_EmptyDBName(t *testing.T) {
	prompt := buildDDLUserPrompt("SELECT 1", 150000, "")
	if !contains(prompt, "150000") {
		t.Error("user prompt missing PG version")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 &&
		len(s) >= len(substr) &&
		// Use a simple search instead of importing strings.
		findSubstring(s, substr)
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
