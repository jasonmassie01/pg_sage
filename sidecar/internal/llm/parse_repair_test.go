package llm

import (
	"strings"
	"testing"
)

// No concurrent access tests: ParseJSON is a pure function.

type repairItem struct {
	X int `json:"x"`
}

// llm-07: the repair now keeps an object wrapper, so the ParseJSON retry
// must still hand an array caller the wrapped array (json_object mode).
func TestParseJSON_TruncatedObjectWrapperIntoArray(t *testing.T) {
	var out []repairItem
	if err := ParseJSON(`{"items":[{"x":1},{"x":2},{"x":`, JSONArray, &out); err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(out) != 2 || out[0].X != 1 || out[1].X != 2 {
		t.Fatalf("out = %+v, want [{1} {2}]", out)
	}
}

// Happy path: an object caller gets the salvaged object, wrapper intact.
func TestParseJSON_TruncatedObjectIntoStruct(t *testing.T) {
	var out struct {
		Findings []repairItem `json:"findings"`
		Note     string       `json:"note"`
	}
	err := ParseJSON("```json\n{\"note\":\"n\",\"findings\":[{\"x\":1},{\"x\":2",
		JSONObject, &out)
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if out.Note != "n" || len(out.Findings) != 1 || out.Findings[0].X != 1 {
		t.Fatalf("out = %+v, want note n and one finding", out)
	}
}

// Error propagation: nothing salvageable still reports both attempts.
func TestParseJSON_UnsalvageableReportsRepairFailure(t *testing.T) {
	var out []repairItem
	err := ParseJSON(`[{"x":`, JSONArray, &out)
	if err == nil || out != nil {
		t.Fatalf("ParseJSON = (%+v, %v), want an error and nil out", out, err)
	}
	if got := err.Error(); !strings.Contains(got, "json unmarshal") ||
		!strings.Contains(got, "repair also failed") {
		t.Fatalf("error %q does not report both attempts", got)
	}
}
