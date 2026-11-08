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

// llm-07: the repair salvages objects as well as arrays, cuts at the last
// completed value and never returns anything that is not valid JSON.
func TestRepairTruncatedJSON_Objects(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"wrapper object kept", `{"findings":[{"x":1},{"x":2`,
			`{"findings":[{"x":1}]}`},
		{"cut number dropped", `{"a":1,"b":[1,2`, `{"a":1,"b":[1]}`},
		{"partial member object", `{"a":{"b":1,"c":2`, `{"a":{"b":1}}`},
		{"closed element without comma", `[{"a":1}`, `[{"a":1}]`},
		{"scalars in array", `[true,false,nu`, `[true,false]`},
		{"preamble before object", "Here you go:\n{\"a\":1,\"b\":2",
			`{"a":1}`},
		{"trailing comma", `{"a":1,`, `{"a":1}`},
		{"whitespace before cut", "{\"a\": [1,\n  2", `{"a": [1]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RepairTruncatedJSON(tc.in)
			if got != tc.want {
				t.Fatalf("RepairTruncatedJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("repaired output %q is not valid JSON", got)
			}
		})
	}
}

// Boundary: structure inside strings is ignored, escaped quotes do not end
// a string, and a cut inside a key drops the whole member.
func TestRepairTruncatedJSON_StringsAndKeys(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"braces and escapes in strings", `{"s":"a{b]\"c,","t":"x`,
			`{"s":"a{b]\"c,"}`},
		{"escaped backslash ends string", `{"a":"x\\","b":"y`, `{"a":"x\\"}`},
		{"cut inside key", `{"a":1,"ke`, `{"a":1}`},
		{"key without colon", `{"a":1,"key"`, `{"a":1}`},
		{"key without value", `{"a":1,"key":`, `{"a":1}`},
		{"cut string value", `{"a":1,"key":"val`, `{"a":1}`},
		{"unicode string", `{"a":"é✓","b":"z`, `{"a":"é✓"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepairTruncatedJSON(tc.in); got != tc.want {
				t.Fatalf("RepairTruncatedJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Nothing salvageable, prose and complete input come back byte-identical.
func TestRepairTruncatedJSON_Unchanged(t *testing.T) {
	for _, in := range []string{
		"",
		"   \n\t ",
		"{",
		"[",
		`{"a"`,
		`{"a":`,
		`{"items":[{"x":1,"y":2`,
		"## Summary\nThe index [critical: {a} and",
		"Indexes: [orders, items, and",
		`{"a":[1,2]}`,
		"  {\"a\":1}\n",
		`[1,2] and then [3,`,
		`"a[1,"`,
		`[1,2,]`,
	} {
		if got := RepairTruncatedJSON(in); got != in {
			t.Errorf("RepairTruncatedJSON(%q) = %q, want the input unchanged", in, got)
		}
	}
}

// The output is either the input, byte for byte, or valid JSON; valid JSON
// input is never rewritten.
func FuzzRepairTruncatedJSON(f *testing.F) {
	for _, seed := range []string{
		`{"findings":[{"x":1},{"x":2`, `{"a":1,"b":[1,2`, `[{"a":"1"},{"b`,
		"## Summary\nThe index [critical: {a} and", `{"s":"a{b]\"c,","t":"x`,
		`"a[1,"`, "", "{", `[1,{"a":[true,null,"x\u00e9"]},-2.5e3,`, `{"a":"\`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := RepairTruncatedJSON(in)
		if got != in && !json.Valid([]byte(got)) {
			t.Fatalf("RepairTruncatedJSON(%q) = %q: changed but not valid JSON", in, got)
		}
		if json.Valid([]byte(in)) && got != in {
			t.Fatalf("valid input %q rewritten to %q", in, got)
		}
	})
}
