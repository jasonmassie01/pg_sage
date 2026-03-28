package llm

import (
	"encoding/json"
	"testing"
)

func TestRepairTruncatedJSON_CompleteArray(t *testing.T) {
	input := `[{"a":"1"},{"b":"2"}]`
	got := RepairTruncatedJSON(input)
	if got != input {
		t.Errorf("complete array should pass through unchanged, got %s", got)
	}
}

func TestRepairTruncatedJSON_TruncatedMidObject(t *testing.T) {
	input := `[{"hint":"HashJoin(t1 t2)","rationale":"good"},{"hint":"Set(work_mem`
	got := RepairTruncatedJSON(input)
	want := `[{"hint":"HashJoin(t1 t2)","rationale":"good"}]`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// Must be valid JSON
	var arr []map[string]any
	if err := json.Unmarshal([]byte(got), &arr); err != nil {
		t.Errorf("repaired JSON should be valid: %v", err)
	}
	if len(arr) != 1 {
		t.Errorf("expected 1 salvaged object, got %d", len(arr))
	}
}

func TestRepairTruncatedJSON_TruncatedMidValue(t *testing.T) {
	input := `[{"directive":"Set(work_mem \"256MB\")","rationale":"high card`
	got := RepairTruncatedJSON(input)
	// No complete object, should return input unchanged
	if got != input {
		t.Errorf("no complete object, should return as-is, got %s", got)
	}
}

func TestRepairTruncatedJSON_MultipleComplete(t *testing.T) {
	input := `[{"a":"1"},{"b":"2"},{"c":"trun`
	got := RepairTruncatedJSON(input)
	want := `[{"a":"1"},{"b":"2"}]`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	var arr []map[string]string
	if err := json.Unmarshal([]byte(got), &arr); err != nil {
		t.Errorf("repaired JSON should be valid: %v", err)
	}
	if len(arr) != 2 {
		t.Errorf("expected 2 objects, got %d", len(arr))
	}
}

func TestRepairTruncatedJSON_NestedBraces(t *testing.T) {
	input := `[{"detail":{"nested":"val"},"top":"ok"},{"partial`
	got := RepairTruncatedJSON(input)
	want := `[{"detail":{"nested":"val"},"top":"ok"}]`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRepairTruncatedJSON_EscapedQuotes(t *testing.T) {
	input := `[{"sql":"SELECT \"col\" FROM t","ok":true},{"trunc`
	got := RepairTruncatedJSON(input)
	want := `[{"sql":"SELECT \"col\" FROM t","ok":true}]`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRepairTruncatedJSON_WithPreamble(t *testing.T) {
	input := "Here is the JSON:\n" +
		`[{"hint":"HashJoin(t1 t2)"},{"hint":"trunc`
	got := RepairTruncatedJSON(input)
	want := `[{"hint":"HashJoin(t1 t2)"}]`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRepairTruncatedJSON_EmptyInput(t *testing.T) {
	got := RepairTruncatedJSON("")
	if got != "" {
		t.Errorf("empty input should return empty, got %s", got)
	}
}

func TestRepairTruncatedJSON_NoArray(t *testing.T) {
	input := `{"single":"object"}`
	got := RepairTruncatedJSON(input)
	if got != input {
		t.Errorf("no array start, should return as-is, got %s", got)
	}
}

func TestIsThinkingModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-2.5-flash", true},
		{"gemini-2.5-pro", true},
		{"gemini-2.5-flash-preview", true},
		{"gemini-2.0-flash", false},
		{"gemini-2.0-flash-thinking-exp", true},
		{"gpt-4o", false},
		{"o1-preview", true},
		{"o3-mini", true},
		{"claude-3.5-sonnet", false},
	}
	for _, tc := range cases {
		got := isThinkingModel(tc.model)
		if got != tc.want {
			t.Errorf("isThinkingModel(%q) = %v, want %v",
				tc.model, got, tc.want)
		}
	}
}
