package snapstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// A catalog document is parsed once per cycle (the json.Valid pre-scan
// doubled the parse). Malformed JSON stays an error the writer reports;
// a valid document a delta cannot express stays errNotEncodable.
func TestParseCatalogErrorKinds(t *testing.T) {
	fields := keyFields["tables"]
	malformed := []string{`[{"schemaname": "a", "relname": "b"}`, `[{]`, `[1,]`,
		`[{"schemaname": "a", "relname": "b"}] trailing`, ``, `   `}
	for _, doc := range malformed {
		_, err := parseCatalog([]byte(doc), fields)
		if err == nil || errors.Is(err, errNotEncodable) {
			t.Fatalf("malformed %q = %v, want a malformed-JSON error", doc, err)
		}
	}
	notEncodable := []string{`{"a": 1}`, `"x"`, `[1, 2]`, `[null]`, `[["a"]]`,
		`[{"schemaname": "a"}]`,
		`[{"schemaname": "a", "relname": "b"}, {"schemaname": "a", "relname": "b"}]`}
	for _, doc := range notEncodable {
		_, err := parseCatalog([]byte(doc), fields)
		if !errors.Is(err, errNotEncodable) {
			t.Fatalf("valid but not encodable %q = %v, want errNotEncodable", doc, err)
		}
	}
	c, err := parseCatalog([]byte(` [{"schemaname": "a", "relname": "b", "n": 1}] `), fields)
	if err != nil || len(c.items) != 1 || string(c.items[0]["n"]) != "1" {
		t.Fatalf("valid document = %+v %v", c, err)
	}
}

func BenchmarkParseCatalogTables(b *testing.B) {
	items := make([]map[string]any, 20000)
	for i := range items {
		item := map[string]any{"schemaname": "app", "relname": fmt.Sprintf("t_%d", i)}
		for f := 0; f < 30; f++ {
			item[fmt.Sprintf("c%02d", f)] = i*31 + f
		}
		items[i] = item
	}
	doc, err := json.Marshal(items)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseCatalog(doc, keyFields["tables"]); err != nil {
			b.Fatal(err)
		}
	}
}
