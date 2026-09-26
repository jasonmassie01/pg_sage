package llm

import (
	"errors"
	"testing"
)

type parsedRec struct {
	DDL             string   `json:"ddl"`
	AffectedQueries []string `json:"affected_queries"`
}

// G3-B09: a truncated array whose complete element contains an inner
// array must still yield the complete element (thinking-model cutoff).
func TestParseJSON_TruncatedArrayWithInnerArraySalvagesElement(t *testing.T) {
	raw := `[{"ddl":"CREATE INDEX a","affected_queries":["q1"]},` +
		`{"ddl":"CREATE INDEX CONC`
	var out []parsedRec
	if err := ParseJSON(raw, JSONArray, &out); err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(out) != 1 || out[0].DDL != "CREATE INDEX a" {
		t.Fatalf("got %+v, want one rec with ddl %q", out, "CREATE INDEX a")
	}
	if len(out[0].AffectedQueries) != 1 || out[0].AffectedQueries[0] != "q1" {
		t.Errorf("affected_queries = %v, want [q1]", out[0].AffectedQueries)
	}
}

// G3-B09: leading prose containing a bracket must not be mistaken for
// the start of the JSON payload.
func TestParseJSON_LeadingProseBracketIgnored(t *testing.T) {
	raw := "I looked at [the plan] and recommend:\n" +
		`[{"ddl":"CREATE INDEX b","affected_queries":[]}]`
	var out []parsedRec
	if err := ParseJSON(raw, JSONArray, &out); err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(out) != 1 || out[0].DDL != "CREATE INDEX b" {
		t.Fatalf("got %+v, want one rec with ddl %q", out, "CREATE INDEX b")
	}
}

// G3-B09: a model in json_object mode answering an array prompt with a
// single object must yield that object as a one-element array, not the
// inner affected_queries array.
func TestParseJSON_SingleObjectForArrayShapeIsWrapped(t *testing.T) {
	raw := `{"ddl":"CREATE INDEX c","affected_queries":["q1"]}`
	var out []parsedRec
	if err := ParseJSON(raw, JSONArray, &out); err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(out) != 1 || out[0].DDL != "CREATE INDEX c" {
		t.Fatalf("got %+v, want one rec with ddl %q", out, "CREATE INDEX c")
	}
}

// G3-B09 / G3-B11: json_object mode wraps array answers in an object
// with a single array-of-objects field; that array is the payload.
func TestParseJSON_ObjectWrappingArrayIsUnwrapped(t *testing.T) {
	raw := `{"recommendations":[{"ddl":"x1"},{"ddl":"x2"}]}`
	var out []parsedRec
	if err := ParseJSON(raw, JSONArray, &out); err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(out) != 2 || out[0].DDL != "x1" || out[1].DDL != "x2" {
		t.Fatalf("got %+v, want [x1 x2]", out)
	}
}

// G3-B09: repair must cut at the last complete top-level element even
// when elements contain nested arrays.
func TestRepairTruncatedJSON_DepthAware(t *testing.T) {
	in := `[{"a":[1,2]},{"a":[3`
	got := RepairTruncatedJSON(in)
	want := `[{"a":[1,2]}]`
	if got != want {
		t.Fatalf("RepairTruncatedJSON(%q) = %q, want %q", in, got, want)
	}
}

// G3-B10: an empty model response is an error, never "nothing to do".
func TestParseJSON_EmptyResponseIsError(t *testing.T) {
	for _, raw := range []string{"", "   \n\t "} {
		var out []parsedRec
		err := ParseJSON(raw, JSONArray, &out)
		if !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("ParseJSON(%q) err = %v, want ErrEmptyResponse", raw, err)
		}
		if out != nil {
			t.Errorf("ParseJSON(%q) out = %v, want nil", raw, out)
		}
	}
}

// Explicit empty containers remain a valid "nothing recommended".
func TestParseJSON_EmptyArrayStillValid(t *testing.T) {
	var out []parsedRec
	if err := ParseJSON("```json\n[]\n```", JSONArray, &out); err != nil {
		t.Fatalf("ParseJSON([]): %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}
