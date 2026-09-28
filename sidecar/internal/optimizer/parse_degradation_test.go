package optimizer

import (
	"errors"
	"github.com/pg-sage/sidecar/internal/llm"
	"strings"
	"testing"
)

// --- parseRecommendations degradation tests ---
// These test the parse function with inputs that simulate real LLM
// failure modes: garbage, truncated JSON, valid JSON with missing
// fields, and partial JSON that stripToJSON cannot recover.

func TestParseRecommendations_CompleteGarbage(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"binary_garbage", "\x00\x01\x02\xff\xfe"},
		{"html_error",
			"<html><body>502 Bad Gateway</body></html>"},
		{"plain_error", "Error: rate limit exceeded"},
		{"number", "42"},
		{"boolean", "true"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRecommendations(tc.input)
			if err == nil {
				t.Errorf("expected error for garbage input %q",
					tc.name)
			}
		})
	}
}

func TestParseRecommendations_TruncatedJSON_NoClosingBracket(
	t *testing.T,
) {
	// stripToJSON finds '[' but no ']', falls through.
	input := `[{"table":"public.orders","ddl":"CREATE INDEX CONCURRENTLY`
	_, err := parseRecommendations(input)
	if err == nil {
		t.Error("expected error for truncated JSON without ]")
	}
}

func TestParseRecommendations_PartialJSON_BracketsMismatch(
	t *testing.T,
) {
	// stripToJSON extracts between [ and ] but content is broken.
	input := `thinking...[{"table":"pub] more text`
	_, err := parseRecommendations(input)
	if err == nil {
		t.Error("expected error for mismatched brackets")
	}
}

func TestParseRecommendations_EmptyArray(t *testing.T) {
	recs, err := parseRecommendations("[]")
	if err != nil {
		t.Fatalf("unexpected error for empty array: %v", err)
	}
	if recs != nil {
		t.Errorf("expected nil for empty array, got %v", recs)
	}
}

// An empty model response is an error, not "no recommendations"
// (G3-B10); this test previously asserted a nil error.
func TestParseRecommendations_EmptyString(t *testing.T) {
	recs, err := parseRecommendations("")
	if !errors.Is(err, llm.ErrEmptyResponse) {
		t.Fatalf("err = %v, want llm.ErrEmptyResponse", err)
	}
	if recs != nil {
		t.Errorf("expected nil for empty string, got %v", recs)
	}
}

func TestParseRecommendations_EmptyObject_MissingFields(t *testing.T) {
	// Array with a completely empty object. JSON unmarshals fine but
	// all fields are zero-valued.
	recs, err := parseRecommendations(`[{}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	// Verify zero-value fields (caller should handle gracefully).
	if recs[0].Table != "" {
		t.Errorf("expected empty Table, got %q", recs[0].Table)
	}
	if recs[0].DDL != "" {
		t.Errorf("expected empty DDL, got %q", recs[0].DDL)
	}
	if recs[0].Confidence != 0 {
		t.Errorf("expected zero Confidence, got %f",
			recs[0].Confidence)
	}
}

func TestParseRecommendations_ValidJSONWrongShape(t *testing.T) {
	// String array - valid JSON but wrong shape for Recommendation.
	_, err := parseRecommendations(`["foo","bar"]`)
	if err == nil {
		t.Error("expected error for string array")
	}
}

func TestParseRecommendations_MarkdownWrappedGarbage(t *testing.T) {
	input := "```json\nSorry, I cannot analyze this.\n```"
	_, err := parseRecommendations(input)
	if err == nil {
		t.Error("expected error for markdown-wrapped garbage")
	}
}

func TestParseRecommendations_ThinkingTokensNoJSON(t *testing.T) {
	input := "Let me analyze the table structure.\n\n" +
		"Based on the query patterns, the existing indexes " +
		"appear adequate."
	_, err := parseRecommendations(input)
	if err == nil {
		t.Error("expected error for thinking-only response")
	}
}

func TestParseRecommendations_ValidJSONPartialFields(t *testing.T) {
	// Only table and DDL populated, rest missing.
	input := `[{"table":"public.orders",` +
		`"ddl":"CREATE INDEX CONCURRENTLY idx ON orders(status)"}]`
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].Table != "public.orders" {
		t.Errorf("expected table 'public.orders', got %q",
			recs[0].Table)
	}
	if recs[0].Severity != "" {
		t.Errorf("expected empty severity for missing field, got %q",
			recs[0].Severity)
	}
}

func TestParseRecommendations_NestedThinkingWithValidJSON(
	t *testing.T,
) {
	// Long thinking prefix followed by valid JSON.
	thinking := strings.Repeat("thinking... ", 100)
	input := thinking + `[{"table":"t","ddl":"d","rationale":"r",` +
		`"severity":"info","index_type":"btree","category":"c",` +
		`"estimated_improvement_pct":15}]` +
		"\n\nThat's my recommendation."
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].Table != "t" {
		t.Errorf("expected table 't', got %q", recs[0].Table)
	}
}

func TestParseRecommendations_ExtraFields_Ignored(t *testing.T) {
	// LLM adds fields not in the struct - should be silently ignored.
	input := `[{"table":"t","ddl":"d","rationale":"r",` +
		`"severity":"info","extra_field":"ignored",` +
		`"another_field":42}]`
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
}
