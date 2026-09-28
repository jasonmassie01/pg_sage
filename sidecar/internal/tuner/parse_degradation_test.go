package tuner

import (
	"errors"
	"github.com/pg-sage/sidecar/internal/llm"
	"strings"
	"testing"
)

// --- parseLLMPrescriptions degradation tests ---
// These test the parse function with inputs that simulate real LLM
// failure modes: garbage output, truncated JSON, valid JSON with
// missing required fields, and partial JSON that stripToJSON cannot
// recover.

func TestParseLLMPrescriptions_CompleteGarbage(t *testing.T) {
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
			_, err := parseLLMPrescriptions(tc.input)
			if err == nil {
				t.Errorf("expected error for garbage input %q",
					tc.name)
			}
		})
	}
}

func TestParseLLMPrescriptions_TruncatedJSON_NoClosingBracket(
	t *testing.T,
) {
	// stripToJSON finds '[' but no ']', falls through.
	input := `[{"hint_directive":"HashJoin(o c)","rationale":"hash join is better for`
	_, err := parseLLMPrescriptions(input)
	if err == nil {
		t.Error("expected error for truncated JSON without ]")
	}
}

func TestParseLLMPrescriptions_PartialJSON_BracketsMismatch(
	t *testing.T,
) {
	// stripToJSON extracts between [ and ] but content is broken.
	input := `thinking...[{"hint_directive":"Ha] more text`
	_, err := parseLLMPrescriptions(input)
	if err == nil {
		t.Error("expected error for mismatched brackets")
	}
}

func TestParseLLMPrescriptions_EmptyObject_MissingFields(
	t *testing.T,
) {
	// Array with a completely empty object. JSON unmarshals fine
	// but all fields are zero-valued.
	recs, err := parseLLMPrescriptions(`[{}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].HintDirective != "" {
		t.Errorf("expected empty HintDirective, got %q",
			recs[0].HintDirective)
	}
	if recs[0].Confidence != 0 {
		t.Errorf("expected zero Confidence, got %f",
			recs[0].Confidence)
	}
}

func TestParseLLMPrescriptions_ValidJSONWrongShape(t *testing.T) {
	// String array - valid JSON but wrong shape.
	_, err := parseLLMPrescriptions(`["foo","bar"]`)
	if err == nil {
		t.Error("expected error for string array")
	}
}

func TestParseLLMPrescriptions_MarkdownWrappedGarbage(t *testing.T) {
	input := "```json\nSorry, I cannot help.\n```"
	_, err := parseLLMPrescriptions(input)
	if err == nil {
		t.Error("expected error for markdown-wrapped garbage")
	}
}

func TestParseLLMPrescriptions_ThinkingTokensNoJSON(t *testing.T) {
	input := "Let me think about the query plan.\n\n" +
		"The current plan looks optimal, no hints needed."
	_, err := parseLLMPrescriptions(input)
	if err == nil {
		t.Error("expected error for thinking-only response")
	}
}

func TestParseLLMPrescriptions_ValidJSONPartialFields(t *testing.T) {
	// Only hint_directive populated, rest missing.
	input := `[{"hint_directive":"HashJoin(o c)"}]`
	recs, err := parseLLMPrescriptions(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].HintDirective != "HashJoin(o c)" {
		t.Errorf("expected 'HashJoin(o c)', got %q",
			recs[0].HintDirective)
	}
	if recs[0].Rationale != "" {
		t.Errorf("expected empty rationale, got %q",
			recs[0].Rationale)
	}
	if recs[0].Confidence != 0 {
		t.Errorf("expected zero confidence, got %f",
			recs[0].Confidence)
	}
}

func TestParseLLMPrescriptions_NestedThinkingWithValidJSON(
	t *testing.T,
) {
	thinking := strings.Repeat("analyzing... ", 100)
	input := thinking +
		`[{"hint_directive":"Set(work_mem \"256MB\")","rationale":"spill to disk","confidence":0.85}]` +
		"\n\nDone."
	recs, err := parseLLMPrescriptions(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].Confidence != 0.85 {
		t.Errorf("expected confidence 0.85, got %f",
			recs[0].Confidence)
	}
}

func TestParseLLMPrescriptions_ExtraFields_Ignored(t *testing.T) {
	input := `[{"hint_directive":"NestLoop(s)",` +
		`"rationale":"small inner","confidence":0.7,` +
		`"unknown_field":"ignored","another":42}]`
	recs, err := parseLLMPrescriptions(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
}

func TestParseLLMPrescriptions_WhitespaceOnly(t *testing.T) {
	recs, err := parseLLMPrescriptions("   \n\t  ")
	// Blank output is llm.ErrEmptyResponse (G3-B10); this test asserted a
	// nil error, which let empty completions look like "no hints".
	if !errors.Is(err, llm.ErrEmptyResponse) {
		t.Fatalf("err = %v, want llm.ErrEmptyResponse", err)
	}
	if recs != nil {
		t.Errorf("expected nil for whitespace, got %v", recs)
	}
}

func TestConvertPrescriptions_EmptyHint_Filtered(t *testing.T) {
	logFn := func(string, string, ...any) {}
	recs := []LLMPrescription{
		{HintDirective: "", Rationale: "should be filtered"},
	}
	out := convertPrescriptions(recs, 0, logFn)
	if len(out) != 0 {
		t.Errorf("expected 0 valid prescriptions, got %d",
			len(out))
	}
}

func TestConvertPrescriptions_ZeroConfidence_StillValid(t *testing.T) {
	logFn := func(string, string, ...any) {}
	recs := []LLMPrescription{
		{
			HintDirective: "HashJoin(o c)",
			Rationale:     "test",
			Confidence:    0,
		},
	}
	out := convertPrescriptions(recs, 0, logFn)
	if len(out) != 1 {
		t.Errorf("expected 1 prescription (zero confidence is "+
			"valid), got %d", len(out))
	}
}

// A single object answered for the array prompt (json_object mode) is
// one prescription, not a parse error (G3-B09); previously it was lost.
func TestParseLLMPrescriptions_ObjectInsteadOfArray(t *testing.T) {
	input := `{"hint_directive":"HashJoin(o c)"}`
	recs, err := parseLLMPrescriptions(input)
	if err != nil || len(recs) != 1 || recs[0].HintDirective != "HashJoin(o c)" {
		t.Errorf("recs=%+v err=%v, want one HashJoin(o c)", recs, err)
	}
}
