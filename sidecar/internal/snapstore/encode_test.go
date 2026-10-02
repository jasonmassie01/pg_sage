package snapstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// indexFields is the identity of an index element.
var indexFields = []string{"schemaname", "indexrelname"}

// deltaDoc decodes a delta payload for assertions.
type deltaDoc struct {
	K []string                              `json:"k"`
	N int                                   `json:"n"`
	U map[string]map[string]json.RawMessage `json:"u"`
	A []json.RawMessage                     `json:"a"`
	D []string                              `json:"d"`
	O []int                                 `json:"o"`
}

func idx(name string, scans int) string {
	return fmt.Sprintf(`{"schemaname":"app","relname":"t","indexrelname":%q,`+
		`"idx_scan":%d,"idx_tup_read":0,"indexdef":"CREATE INDEX %s ON app.t (c)",`+
		`"index_type":"btree","indisvalid":true}`, name, scans, name)
}

func list(items ...string) []byte { return []byte("[" + strings.Join(items, ",") + "]") }

func mustCatalog(t *testing.T, data []byte) *catalog {
	t.Helper()
	c, err := parseCatalog(data, indexFields)
	if err != nil {
		t.Fatalf("parseCatalog(%s): %v", data, err)
	}
	return c
}

func mustDelta(t *testing.T, base, cur []byte) (deltaDoc, []byte) {
	t.Helper()
	raw, err := encodeDelta(mustCatalog(t, base), mustCatalog(t, cur), indexFields)
	if err != nil {
		t.Fatalf("encodeDelta: %v", err)
	}
	var d deltaDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("delta is not JSON: %s: %v", raw, err)
	}
	return d, raw
}

func key(parts ...string) string { return strings.Join(parts, keySeparator) }

// Happy path: a counter change carries only the changed field, keyed by
// identity; the static definition is not repeated.
func TestEncodeDelta_CounterChangeCarriesOnlyChangedFields(t *testing.T) {
	base := list(idx("a", 0), idx("b", 5), idx("c", 0))
	cur := list(idx("a", 0), idx("b", 9), idx("c", 0))
	d, raw := mustDelta(t, base, cur)
	want := `{"k":["schemaname","indexrelname"],"n":3,"u":{"app` + "\\u001f" +
		`b":{"idx_scan":9}}}`
	if string(raw) != want {
		t.Fatalf("delta = %s\nwant    %s", raw, want)
	}
	if strings.Contains(string(raw), "CREATE INDEX") {
		t.Fatal("delta repeats the static definition")
	}
	if len(d.A) != 0 || len(d.D) != 0 || d.O != nil {
		t.Fatalf("unexpected adds/removes/order: %+v", d)
	}
}

// Zero: nothing changed is the smallest possible delta, still with the
// identity fields and the element count.
func TestEncodeDelta_NoChangeIsEmptyDelta(t *testing.T) {
	base := list(idx("a", 1), idx("b", 2))
	_, raw := mustDelta(t, base, base)
	if want := `{"k":["schemaname","indexrelname"],"n":2}`; string(raw) != want {
		t.Fatalf("delta = %s, want %s", raw, want)
	}
}

// A new element (a new oid sorts last) is carried in full and appended.
func TestEncodeDelta_AddedElementIsAppendedInFull(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1)), list(idx("a", 1), idx("z", 0)))
	if d.N != 2 || len(d.A) != 1 || d.O != nil || len(d.U) != 0 {
		t.Fatalf("delta = %+v, want one appended element", d)
	}
	if !strings.Contains(string(d.A[0]), `"indexdef":"CREATE INDEX z`) {
		t.Fatalf("added element lost its definition: %s", d.A[0])
	}
}

// A dropped element is listed by key; the order needs no permutation.
func TestEncodeDelta_RemovedElementIsListedByKey(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1), idx("b", 2), idx("c", 3)),
		list(idx("a", 1), idx("c", 3)))
	if !reflect.DeepEqual(d.D, []string{key("app", "b")}) || d.N != 2 || d.O != nil {
		t.Fatalf("delta = %+v, want b removed", d)
	}
}

// A reordered list (sequences by use, queries by time) carries an explicit
// order: indices into the base followed by the added elements.
func TestEncodeDelta_ReorderCarriesExplicitOrder(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1), idx("b", 2), idx("c", 3)),
		list(idx("c", 3), idx("a", 1), idx("b", 2)))
	if !reflect.DeepEqual(d.O, []int{2, 0, 1}) || len(d.U) != 0 || d.N != 3 {
		t.Fatalf("order = %v (delta %+v), want [2 0 1]", d.O, d)
	}
}

// A rename keeps its position (same oid): the new key is an added element
// placed where the old one was, so the order is explicit.
func TestEncodeDelta_RenameInPlaceKeepsPosition(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1), idx("b", 2), idx("c", 3)),
		list(idx("a", 1), idx("b2", 2), idx("c", 3)))
	if !reflect.DeepEqual(d.O, []int{0, 3, 2}) || len(d.A) != 1 || d.N != 3 {
		t.Fatalf("delta = %+v, want order [0 3 2] with b2 added", d)
	}
}

// Boundary: an element can lose a field only through a full row (a patch
// merges fields and cannot delete one).
func TestEncodeDelta_FieldRemovedIsNotEncodable(t *testing.T) {
	base := mustCatalog(t, list(idx("a", 1)))
	cur := mustCatalog(t, []byte(`[{"schemaname":"app","indexrelname":"a"}]`))
	_, err := encodeDelta(base, cur, indexFields)
	if !errors.Is(err, errNotEncodable) || !strings.Contains(err.Error(), "idx_scan") {
		t.Fatalf("err = %v, want errNotEncodable naming the lost field", err)
	}
}

// Empty current list: every base element is removed and the count is 0.
func TestEncodeDelta_EmptyCurrentRemovesEverything(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1), idx("b", 2)), []byte(`[]`))
	if d.N != 0 || len(d.D) != 2 || len(d.A) != 0 {
		t.Fatalf("delta = %+v, want n=0 with both removed", d)
	}
}

// Empty base list: everything is added.
func TestEncodeDelta_EmptyBaseAddsEverything(t *testing.T) {
	d, _ := mustDelta(t, []byte(`[]`), list(idx("a", 1), idx("b", 2)))
	if d.N != 2 || len(d.A) != 2 || len(d.D) != 0 || d.O != nil {
		t.Fatalf("delta = %+v, want two added", d)
	}
}

// Invalid input: only an array of objects with unique, present, scalar
// identities can be delta encoded; anything else is stored in full.
func TestParseCatalog_RejectsWhatADeltaCannotExpress(t *testing.T) {
	cases := map[string]string{
		"null":           `null`,
		"object":         `{"schemaname":"app"}`,
		"scalar":         `42`,
		"non-object":     `[1,2]`,
		"duplicate key":  `[` + idx("a", 1) + `,` + idx("a", 2) + `]`,
		"missing field":  `[{"schemaname":"app"}]`,
		"null key":       `[{"schemaname":"app","indexrelname":null}]`,
		"bool key":       `[{"schemaname":"app","indexrelname":true}]`,
		"float key":      `[{"schemaname":"app","indexrelname":1.5}]`,
		"object key":     `[{"schemaname":"app","indexrelname":{}}]`,
		"separator used": `[{"schemaname":"a\u001fb","indexrelname":"c"}]`,
	}
	for name, doc := range cases {
		if _, err := parseCatalog([]byte(doc), indexFields); !errors.Is(err, errNotEncodable) {
			t.Errorf("%s: err = %v, want errNotEncodable", name, err)
		}
	}
	if _, err := parseCatalog([]byte(`[{"schemaname":`), indexFields); err == nil {
		t.Error("malformed JSON parsed")
	}
}

// Integer identities (queryid) keep full int64 precision: a float64 round
// trip would merge 2^53+1 into 2^53.
func TestParseCatalog_IntegerKeyKeepsPrecision(t *testing.T) {
	c, err := parseCatalog([]byte(`[{"queryid":9007199254740993},`+
		`{"queryid":9007199254740992},{"queryid":-5}]`), []string{"queryid"})
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	want := []string{"9007199254740993", "9007199254740992", "-5"}
	if !reflect.DeepEqual(c.keys, want) {
		t.Fatalf("keys = %q, want %q", c.keys, want)
	}
}

// A string identity is compared unescaped, the way jsonb's ->> returns it
// (Go escapes "<" as <), and the fields are joined by the separator.
func TestParseCatalog_StringKeyIsUnescaped(t *testing.T) {
	raw, _ := json.Marshal([]map[string]string{{"schemaname": "a<b", "indexrelname": "x&y"}})
	c, err := parseCatalog(raw, indexFields)
	if err != nil {
		t.Fatalf("parseCatalog(%s): %v", raw, err)
	}
	if want := key("a<b", "x&y"); c.keys[0] != want {
		t.Fatalf("key = %q, want %q", c.keys[0], want)
	}
}

// Every delta category has a key of one to three fields, which is what the
// SQL decoder (sage.snapshot_apply) reads.
func TestKeyFields_EveryCategoryFitsTheDecoder(t *testing.T) {
	want := []string{"foreign_keys", "indexes", "partitions", "queries", "sequences", "tables"}
	var got []string
	for cat, fields := range keyFields {
		got = append(got, cat)
		if len(fields) < 1 || len(fields) > 3 {
			t.Errorf("%s: %d key fields, want 1-3", cat, len(fields))
		}
	}
	if !sameSet(got, want) {
		t.Fatalf("delta categories = %v, want %v", got, want)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
