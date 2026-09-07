package llm

import (
	"strings"
	"testing"
)

func TestStripJSON_ObjectFromProse(t *testing.T) {
	input := "here is the answer: {\"a\":1} — cheers"
	got := StripJSON(input, JSONObject)
	if got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestStripJSON_ArrayFromFences(t *testing.T) {
	input := "```json\n[{\"x\":1}]\n```"
	got := StripJSON(input, JSONArray)
	if got != `[{"x":1}]` {
		t.Errorf("got %q", got)
	}
}

func TestStripJSON_AutoPrefersObject(t *testing.T) {
	input := `{"ok":true} [ignore this]`
	got := StripJSON(input, JSONAuto)
	if !strings.HasPrefix(got, "{") {
		t.Errorf("auto should prefer object, got %q", got)
	}
}

func TestStripJSON_NoDelimiters(t *testing.T) {
	input := "just some prose"
	got := StripJSON(input, JSONArray)
	if got != input {
		t.Errorf("expected input unchanged, got %q", got)
	}
}

func TestParseJSON_ArrayHappyPath(t *testing.T) {
	type rec struct {
		Name string `json:"name"`
	}
	var out []rec
	err := ParseJSON(`[{"name":"a"},{"name":"b"}]`, JSONArray, &out)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Errorf("got %+v", out)
	}
}

func TestParseJSON_ObjectHappyPath(t *testing.T) {
	type rec struct {
		Val int `json:"val"`
	}
	var out rec
	if err := ParseJSON(`{"val":42}`, JSONObject, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Val != 42 {
		t.Errorf("got %d", out.Val)
	}
}

func TestParseJSON_EmptyArrayLeavesNilSlice(t *testing.T) {
	var out []int
	if err := ParseJSON("[]", JSONArray, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil slice, got %v", out)
	}
}

func TestParseJSON_EmptyObjectLeavesZeroStruct(t *testing.T) {
	type rec struct {
		Val int `json:"val"`
	}
	var out rec
	if err := ParseJSON("{}", JSONObject, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Val != 0 {
		t.Errorf("expected zero struct, got %+v", out)
	}
}

func TestParseJSON_ShapeMismatchReturnsError(t *testing.T) {
	// Caller expected an array but the model returned an object.
	var out []int
	err := ParseJSON("{}", JSONArray, &out)
	if err == nil {
		t.Fatal("expected error for shape mismatch")
	}
}

func TestParseJSON_MarkdownFenced(t *testing.T) {
	type rec struct {
		K string `json:"k"`
	}
	var out []rec
	raw := "```json\n[{\"k\":\"v\"}]\n```"
	if err := ParseJSON(raw, JSONArray, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].K != "v" {
		t.Errorf("got %+v", out)
	}
}

func TestParseJSON_RepairsTruncatedArray(t *testing.T) {
	// Mid-object truncation — repair should recover the first two.
	type rec struct {
		N int `json:"n"`
	}
	raw := `[{"n":1},{"n":2},{"n":`
	var out []rec
	if err := ParseJSON(raw, JSONArray, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 2 || out[0].N != 1 || out[1].N != 2 {
		t.Errorf("repair failed: got %+v", out)
	}
}

func TestParseJSON_InvalidJSONErrorIncludesSnippet(t *testing.T) {
	var out []int
	err := ParseJSON("not json at all", JSONArray, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "json unmarshal") {
		t.Errorf("error message unexpected: %v", err)
	}
}

func TestParseJSON_EmptyInputNoError(t *testing.T) {
	var out []int
	if err := ParseJSON("", JSONArray, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil, got %v", out)
	}
}
