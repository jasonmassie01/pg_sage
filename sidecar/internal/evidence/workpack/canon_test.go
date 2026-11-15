package workpack

import (
	"math"
	"strings"
	"testing"
)

// evid-02a: section files are canonical JSON: keys sorted, floats as
// strconv.FormatFloat(v, 'g', -1, 64), integers as written. The same value
// always gives the same bytes. No DB or concurrency test: pure encoding.

func TestCanonical_SortsKeysAndFormatsNumbers(t *testing.T) {
	v := map[string]any{"b": 1.5, "a": []any{int64(3), 2.25e-7, 1e21}, "c": nil,
		"d": map[string]any{"z": true, "y": "<x>"}}
	got, err := canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[3,2.25e-07,1e+21],"b":1.5,"c":null,"d":{"y":"\u003cx\u003e","z":true}}` +
		"\n"
	if string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonical_StructFieldsAreSortedToo(t *testing.T) {
	type s struct {
		Zeta  int     `json:"zeta"`
		Alpha float64 `json:"alpha"`
	}
	got, err := canonical(s{Zeta: 1, Alpha: 0.1})
	if err != nil || string(got) != `{"alpha":0.1,"zeta":1}`+"\n" {
		t.Fatalf("canonical = %s (%v)", got, err)
	}
}

func TestCanonical_RefusesNaN(t *testing.T) {
	if _, err := canonical(map[string]any{"x": math.NaN()}); err == nil ||
		!strings.Contains(err.Error(), "encode") {
		t.Fatalf("NaN: %v, want an encode error", err)
	}
}

func TestCanonical_LargeIntegersStayExact(t *testing.T) {
	got, err := canonical(map[string]any{"q": int64(-9223372036854775808)})
	if err != nil || string(got) != `{"q":-9223372036854775808}`+"\n" {
		t.Fatalf("canonical = %s (%v)", got, err)
	}
}

func TestContentHash_IsOrderIndependentAndNameSensitive(t *testing.T) {
	a := contentSHA256(map[string]string{"a.json": "11", "b.json": "22"})
	b := contentSHA256(map[string]string{"b.json": "22", "a.json": "11"})
	c := contentSHA256(map[string]string{"a.json": "22", "b.json": "11"})
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("hashes %s %s %s: want equal for the same lines, different otherwise",
			a, b, c)
	}
}
