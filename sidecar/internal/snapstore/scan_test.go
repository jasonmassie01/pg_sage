package snapstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// Snapshot documents are arrays of flat objects; the scanner splits them
// and their objects by slicing the (validated) text instead of decoding
// through reflection (nightly perf gate: decoding was the sidecar's
// largest CPU cost). It must agree exactly with encoding/json.

func stdElements(t testing.TB, doc []byte) []json.RawMessage {
	t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal(doc, &raw); err != nil {
		t.Fatalf("std split %s: %v", doc, err)
	}
	return raw
}

func stdObject(t testing.TB, text []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(text, &m); err != nil {
		t.Fatalf("std object %s: %v", text, err)
	}
	return m
}

func sameObject(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

var scanCases = []string{
	`[]`, ` [ ] `, `[{}]`, `[ {} , { } ]`,
	`[{"a":1,"b":"x","c":null,"d":true,"e":false,"f":-1.5e3}]`,
	`[{"a" : 1 , "b" : [1, {"x": [2]}], "c": {"n": {"m": "}"}}}]`,
	`[{"s":"quote \" brace } bracket ] comma , colon :","t":"\\"}]`,
	`[{"u":"é中😀","v":"tab\tnl\n"}]`,
	`[{"dup":1,"dup":2}]`,
	"[\n\t{\n\t\t\"a\": 1\n\t},\n\t{\"b\": 2}\n]",
	`[{"schemaname":"public","relname":"t","n_live_tup":9007199254740993}]`,
}

func TestSplitArrayMatchesEncodingJSON(t *testing.T) {
	for _, doc := range scanCases {
		got, err := splitArray([]byte(doc))
		if err != nil {
			t.Fatalf("split %s: %v", doc, err)
		}
		want := stdElements(t, []byte(doc))
		if len(got) != len(want) {
			t.Fatalf("split %s: %d elements, want %d", doc, len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("split %s element %d = %s, want %s", doc, i, got[i], want[i])
			}
		}
	}
}

func TestScanObjectMatchesEncodingJSON(t *testing.T) {
	for _, doc := range scanCases {
		for _, el := range stdElements(t, []byte(doc)) {
			got, ok := scanObject(el, 0)
			if !ok {
				t.Fatalf("scan %s: not scanned", el)
			}
			if want := stdObject(t, el); !sameObject(got, want) {
				t.Fatalf("scan %s = %v, want %v", el, got, want)
			}
		}
	}
}

// Keys with escapes are left to encoding/json (it unescapes them), and so
// is anything that is not an object.
func TestScanObjectDefersWhatItDoesNotHandle(t *testing.T) {
	for _, text := range []string{`{"a\"b":1}`, `{"\` + `u0061":1}`, `1`, `null`, `[1]`,
		`"x"`, `true`} {
		if _, ok := scanObject([]byte(text), 0); ok {
			t.Fatalf("scanObject(%s) claimed it", text)
		}
	}
}

// Malformed documents are still malformed, valid non-arrays still not
// encodable: splitArray validates before it slices.
func TestSplitArrayErrorKinds(t *testing.T) {
	for _, doc := range []string{``, ` `, `[`, `[{"a":1}`, `[1,]`, `[{"a":}]`,
		`[{"a":1}] x`, `[{"a":"unterminated]`} {
		if _, err := splitArray([]byte(doc)); err == nil ||
			strings.Contains(err.Error(), errNotEncodable.Error()) {
			t.Fatalf("malformed %q = %v, want a malformed-JSON error", doc, err)
		}
	}
	for _, doc := range []string{`{"a":1}`, `"x"`, `1`, `null`} {
		if _, err := splitArray([]byte(doc)); err == nil ||
			!strings.Contains(err.Error(), errNotEncodable.Error()) {
			t.Fatalf("non-array %q = %v, want errNotEncodable", doc, err)
		}
	}
}

// Random documents of nested values, escaped strings and odd spacing.
func TestScannerAgreesWithEncodingJSONOnRandomDocuments(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for n := 0; n < 3000; n++ {
		doc := randomDocument(rng)
		got, err := splitArray(doc)
		if err != nil {
			t.Fatalf("split %s: %v", doc, err)
		}
		want := stdElements(t, doc)
		if len(got) != len(want) {
			t.Fatalf("split %s: %d vs %d", doc, len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("split %s element %d: %s vs %s", doc, i, got[i], want[i])
			}
			if obj, ok := scanObject(got[i], 4); ok && !sameObject(obj, stdObject(t, got[i])) {
				t.Fatalf("scan %s = %v", got[i], obj)
			}
		}
	}
}

func randomDocument(rng *rand.Rand) []byte {
	var sb strings.Builder
	sp := func() {
		sb.WriteString([]string{"", "", " ", "\n", "\t ", "  "}[rng.Intn(6)])
	}
	sb.WriteString("[")
	for i, n := 0, rng.Intn(5); i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sp()
		writeRandomValue(&sb, rng, 0, true)
		sp()
	}
	sb.WriteString("]")
	return []byte(sb.String())
}

func writeRandomValue(sb *strings.Builder, rng *rand.Rand, depth int, object bool) {
	kind := rng.Intn(7)
	if object || depth == 0 {
		kind = 0
	}
	if depth > 3 && kind <= 1 {
		kind = 2
	}
	switch kind {
	case 0:
		sb.WriteString("{")
		keys := []string{"a", "b", "name", "n", "x_y", "a"}
		for i, n := 0, rng.Intn(5); i < n; i++ {
			if i > 0 {
				sb.WriteString(" ,")
			}
			fmt.Fprintf(sb, "%q :", keys[rng.Intn(len(keys))])
			writeRandomValue(sb, rng, depth+1, false)
		}
		sb.WriteString("}")
	case 1:
		sb.WriteString("[")
		for i, n := 0, rng.Intn(4); i < n; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			writeRandomValue(sb, rng, depth+1, false)
		}
		sb.WriteString("]")
	case 2:
		b, _ := json.Marshal([]string{`plain`, `q"uote`, `back\slash`, `}]{[,:`, "é",
			"\x01ctl"}[rng.Intn(6)])
		sb.Write(b)
	case 3:
		sb.WriteString([]string{"0", "-1", "12345678901234567890", "1.5e-3", "-0.0"}[rng.Intn(5)])
	default:
		sb.WriteString([]string{"true", "false", "null"}[rng.Intn(3)])
	}
}

func FuzzScanner(f *testing.F) {
	for _, c := range scanCases {
		f.Add([]byte(c))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		got, err := splitArray(doc)
		var want []json.RawMessage
		stdErr := json.Unmarshal(doc, &want)
		switch {
		case !json.Valid(doc) && err == nil:
			t.Fatalf("malformed %q accepted", doc)
		case stdErr != nil && err == nil:
			t.Fatalf("non-array %q split", doc)
		case stdErr == nil && err != nil && bytes.TrimSpace(doc)[0] == '[':
			// encoding/json also accepts null as an empty array; the
			// catalog parser never did (not an array).
			t.Fatalf("array %q rejected: %v", doc, err)
		}
		if err != nil {
			return
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("split %q element %d", doc, i)
			}
			var m map[string]json.RawMessage
			if obj, ok := scanObject(got[i], 4); ok {
				if json.Unmarshal(got[i], &m) != nil || !sameObject(obj, m) {
					t.Fatalf("scan %q disagrees", got[i])
				}
			}
		}
	})
}

func BenchmarkParseCatalogChangedTables(b *testing.B) {
	items := make([]map[string]any, 5000)
	for i := range items {
		item := map[string]any{"schemaname": "app", "relname": fmt.Sprintf("t_%d", i)}
		for f := 0; f < 30; f++ {
			item[fmt.Sprintf("c%02d", f)] = i*31 + f
		}
		items[i] = item
	}
	doc, _ := json.Marshal(items)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseCatalog(doc, keyFields["tables"], nil); err != nil {
			b.Fatal(err)
		}
	}
}
