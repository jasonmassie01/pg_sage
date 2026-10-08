package snapstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// Most elements of a catalog document (indexes, sequences) are
// byte-identical cycle to cycle. Parsing reuses the previous document's
// parse of an identical element instead of decoding it again (nightly perf
// gate: CPU per cycle 600.3 ms against 600 at 20,000 relations). The result
// must equal a fresh parse in every case.

func reuseDoc(n int, changed map[int]int) []byte {
	items := make([]map[string]any, n)
	for i := range items {
		v := i * 7
		if d, ok := changed[i]; ok {
			v += d
		}
		items[i] = map[string]any{"schemaname": "app", "indexrelname": fmt.Sprintf("i_%d", i),
			"idx_scan": v, "note": "x"}
	}
	b, _ := json.Marshal(items)
	return b
}

func samePointer(a, b map[string]json.RawMessage) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestParseCatalogReusesUnchangedElements(t *testing.T) {
	fields := keyFields["indexes"]
	prev, err := parseCatalog(reuseDoc(50, nil), fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := reuseDoc(50, map[int]int{3: 1, 40: 5})
	cur, err := parseCatalog(doc, fields, prev)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := parseCatalog(doc, fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cur.items, fresh.items) || !reflect.DeepEqual(cur.keys, fresh.keys) ||
		!reflect.DeepEqual(cur.pos, fresh.pos) {
		t.Fatal("a reusing parse differs from a fresh one")
	}
	for i := range cur.items {
		reused := samePointer(cur.items[i], prev.items[i])
		if want := i != 3 && i != 40; reused != want {
			t.Fatalf("element %d reused = %t, want %t", i, reused, want)
		}
	}
}

// Reuse is by content, not position: elements that moved are reused too.
func TestParseCatalogReusesMovedElements(t *testing.T) {
	fields := keyFields["indexes"]
	prev, err := parseCatalog([]byte(`[{"schemaname":"a","indexrelname":"x","n":1},
		{"schemaname":"a","indexrelname":"y","n":2}]`), fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := parseCatalog([]byte(`[{"schemaname":"a","indexrelname":"y","n":2},
		{"schemaname":"a","indexrelname":"z","n":3},{"schemaname":"a","indexrelname":"x","n":1}]`),
		fields, prev)
	if err != nil {
		t.Fatal(err)
	}
	if !samePointer(cur.items[0], prev.items[1]) || !samePointer(cur.items[2], prev.items[0]) {
		t.Fatal("moved identical elements were parsed again")
	}
	if cur.pos[cur.keys[1]] != 1 || string(cur.items[1]["n"]) != "3" {
		t.Fatalf("new element = %v", cur.items[1])
	}
}

// Validation is unchanged by reuse: a malformed document is still an
// error, a duplicate identity or a non-object element still not encodable
// (even when the element is identical to one in the previous document).
func TestParseCatalogReuseKeepsValidation(t *testing.T) {
	fields := keyFields["indexes"]
	prev, err := parseCatalog([]byte(`[{"schemaname":"a","indexrelname":"x"}]`), fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCatalog([]byte(`[{"schemaname":"a","indexrelname":"x"}`), fields,
		prev); err == nil || errors.Is(err, errNotEncodable) {
		t.Fatalf("malformed with a previous parse = %v", err)
	}
	for _, doc := range []string{
		`[{"schemaname":"a","indexrelname":"x"},{"schemaname":"a","indexrelname":"x"}]`,
		`[{"schemaname":"a","indexrelname":"x"}, 1]`, `[null]`, `{"a":1}`,
		`[{"schemaname":"a"}]`} {
		if _, err := parseCatalog([]byte(doc), fields, prev); !errors.Is(err, errNotEncodable) {
			t.Fatalf("%s with a previous parse = %v, want errNotEncodable", doc, err)
		}
	}
}

// Whitespace inside an element makes it a different text: it is parsed
// again, and parses to the same values.
func TestParseCatalogReuseIsByExactText(t *testing.T) {
	fields := keyFields["indexes"]
	prev, err := parseCatalog([]byte(`[{"schemaname":"a","indexrelname":"x","n":1}]`),
		fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := parseCatalog([]byte(`[{"schemaname":"a", "indexrelname":"x","n":1}]`),
		fields, prev)
	if err != nil {
		t.Fatal(err)
	}
	if samePointer(cur.items[0], prev.items[0]) {
		t.Fatal("a differently spelled element was reused")
	}
	if !reflect.DeepEqual(cur.items, prev.items) {
		t.Fatalf("values differ: %v vs %v", cur.items, prev.items)
	}
}

func BenchmarkParseCatalogReuse(b *testing.B) {
	fields := keyFields["indexes"]
	changed := map[int]int{}
	for i := 0; i < 20000; i += 100 {
		changed[i] = 1
	}
	prev, err := parseCatalog(reuseDoc(20000, nil), fields, nil)
	if err != nil {
		b.Fatal(err)
	}
	doc := reuseDoc(20000, changed)
	b.Run("fresh", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := parseCatalog(doc, fields, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("reuse", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := parseCatalog(doc, fields, prev); err != nil {
				b.Fatal(err)
			}
		}
	})
}
