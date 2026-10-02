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
	N int                                   `json:"n"`
	G map[string]json.RawMessage            `json:"g"`
	I map[string]map[string]json.RawMessage `json:"i"`
	U map[string]map[string]json.RawMessage `json:"u"`
	A []json.RawMessage                     `json:"a"`
	D []int                                 `json:"d"`
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
	raw, err := encodeDelta(mustCatalog(t, base), mustCatalog(t, cur))
	if err != nil {
		t.Fatalf("encodeDelta: %v", err)
	}
	var d deltaDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("delta is not JSON: %s: %v", raw, err)
	}
	return d, raw
}

// Happy path: a counter change carries only the changed field, addressed
// by its base index, as an increment; the definition is not repeated.
func TestEncodeDelta_CounterChangeCarriesOnlyChangedFields(t *testing.T) {
	base := list(idx("a", 0), idx("b", 5), idx("c", 0))
	cur := list(idx("a", 0), idx("b", 9), idx("c", 0))
	d, raw := mustDelta(t, base, cur)
	if want := `{"n":3,"i":{"1":{"idx_scan":4}}}`; string(raw) != want {
		t.Fatalf("delta = %s\nwant    %s", raw, want)
	}
	if len(d.A) != 0 || len(d.D) != 0 || d.O != nil || len(d.U) != 0 || len(d.G) != 0 {
		t.Fatalf("unexpected parts: %+v", d)
	}
}

// Integer fields are stored as increments (a counter reset is a negative
// one, values beyond int64 stay exact); every other change (text, bool,
// float, null to a number) is stored as the new value.
func TestEncodeDelta_IntegerIncrementsAndValues(t *testing.T) {
	base := []byte(`[{"schemaname":"app","indexrelname":"a","idx_scan":9,` +
		`"big":99999999999999999999,"def":"x","valid":true,"ratio":1.5,"bytes":null}]`)
	cur := []byte(`[{"schemaname":"app","indexrelname":"a","idx_scan":0,` +
		`"big":100000000000000000001,"def":"y","valid":false,"ratio":2.5,"bytes":8192}]`)
	d, _ := mustDelta(t, base, cur)
	// With one element, every moved counter moved alike: the increments are
	// global (one entry each, smaller than a per-element one).
	if string(d.G["idx_scan"]) != "-9" || string(d.G["big"]) != "2" || len(d.G) != 2 ||
		len(d.I) != 0 {
		t.Fatalf("g = %v, i = %v; want idx_scan -9 and big 2 globally", d.G, d.I)
	}
	want := map[string]string{"def": `"y"`, "valid": "false", "ratio": "2.5", "bytes": "8192"}
	if len(d.U["0"]) != len(want) {
		t.Fatalf("values = %v, want %v", d.U["0"], want)
	}
	for f, v := range want {
		if string(d.U["0"][f]) != v {
			t.Errorf("value %s = %s, want %s", f, d.U["0"][f], v)
		}
	}
}

func tableRow(name string, xidAge, ins int) string {
	return fmt.Sprintf(`{"schemaname":"app","relname":%q,"xid_age":%d,"n_tup_ins":%d,`+
		`"last_vacuum":null}`, name, xidAge, ins)
}

func tablesDoc(xidAges, ins []int) []byte {
	var items []string
	for i := range xidAges {
		items = append(items, tableRow(fmt.Sprintf("t%d", i), xidAges[i], ins[i]))
	}
	return list(items...)
}

func mustTablesDelta(t *testing.T, base, cur []byte) deltaDoc {
	t.Helper()
	fields := []string{"schemaname", "relname"}
	b, err1 := parseCatalog(base, fields)
	c, err2 := parseCatalog(cur, fields)
	if err1 != nil || err2 != nil {
		t.Fatalf("parse: %v / %v", err1, err2)
	}
	raw, err := encodeDelta(b, c)
	if err != nil {
		t.Fatalf("encodeDelta: %v", err)
	}
	var d deltaDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("delta: %v", err)
	}
	return d
}

// A counter that moves alike on most elements (xid_age) is one global
// increment; elements that moved otherwise (a vacuumed table, one left
// alone) carry the difference from it; a counter mostly unchanged gets no
// global increment.
func TestEncodeDelta_GlobalIncrement(t *testing.T) {
	base := tablesDoc([]int{100, 200, 300, 400, 500}, []int{1, 1, 1, 1, 1})
	cur := tablesDoc([]int{137, 237, 337, 10, 500}, []int{1, 1, 5, 1, 1})
	d := mustTablesDelta(t, base, cur)
	if !reflect.DeepEqual(d.G, map[string]json.RawMessage{"xid_age": json.RawMessage("37")}) {
		t.Fatalf("g = %v, want xid_age 37 only", d.G)
	}
	if string(d.I["3"]["xid_age"]) != "-427" || string(d.I["4"]["xid_age"]) != "-37" ||
		string(d.I["2"]["n_tup_ins"]) != "4" || len(d.I) != 3 {
		t.Fatalf("i = %v, want the vacuumed, the untouched and the inserted table", d.I)
	}
	if _, moved := d.I["0"]; moved {
		t.Fatal("an element that moved by exactly the global increment carries an entry")
	}
}

// Boundary: a global increment must beat "unchanged" strictly; a tie or a
// minority leaves every element to carry its own increment.
func TestEncodeDelta_GlobalIncrementNeedsMajority(t *testing.T) {
	base := tablesDoc([]int{1, 2, 3, 4}, []int{0, 0, 0, 0})
	cur := tablesDoc([]int{11, 12, 3, 4}, []int{0, 0, 0, 0})
	if d := mustTablesDelta(t, base, cur); len(d.G) != 0 || len(d.I) != 2 {
		t.Fatalf("tie: g = %v, i = %v; want no global increment", d.G, d.I)
	}
	cur = tablesDoc([]int{11, 12, 13, 4}, []int{0, 0, 0, 0})
	if d := mustTablesDelta(t, base, cur); string(d.G["xid_age"]) != "10" || len(d.I) != 1 {
		t.Fatalf("majority: g = %v, i = %v; want 10 globally, one correction", d.G, d.I)
	}
}

// A field some base element holds in exponent form never gets a global
// increment: jsonb prints 1e+21 as plain digits, so the decoder would add
// to it while the encoder would not.
func TestEncodeDelta_ExponentNumbersBlockGlobalIncrement(t *testing.T) {
	base := []byte(`[{"schemaname":"app","relname":"a","n":1e+21},` +
		`{"schemaname":"app","relname":"b","n":1},{"schemaname":"app","relname":"c","n":1}]`)
	cur := []byte(`[{"schemaname":"app","relname":"a","n":1e+21},` +
		`{"schemaname":"app","relname":"b","n":2},{"schemaname":"app","relname":"c","n":2}]`)
	if d := mustTablesDelta(t, base, cur); len(d.G) != 0 || len(d.I) != 2 {
		t.Fatalf("g = %v, i = %v; want per-element increments only", d.G, d.I)
	}
}

// Zero: nothing changed is the smallest possible delta: the element count.
func TestEncodeDelta_NoChangeIsEmptyDelta(t *testing.T) {
	base := list(idx("a", 1), idx("b", 2))
	_, raw := mustDelta(t, base, base)
	if want := `{"n":2}`; string(raw) != want {
		t.Fatalf("delta = %s, want %s", raw, want)
	}
}

// A new element (a new oid sorts last) is carried in full and appended.
func TestEncodeDelta_AddedElementIsAppendedInFull(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1)), list(idx("a", 1), idx("z", 0)))
	if d.N != 2 || len(d.A) != 1 || d.O != nil || len(d.U) != 0 || len(d.I) != 0 {
		t.Fatalf("delta = %+v, want one appended element", d)
	}
	if !strings.Contains(string(d.A[0]), `"indexdef":"CREATE INDEX z`) {
		t.Fatalf("added element lost its definition: %s", d.A[0])
	}
}

// A dropped element is listed by base index; no permutation is needed.
func TestEncodeDelta_RemovedElementIsListedByIndex(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1), idx("b", 2), idx("c", 3)),
		list(idx("a", 1), idx("c", 3)))
	if !reflect.DeepEqual(d.D, []int{1}) || d.N != 2 || d.O != nil {
		t.Fatalf("delta = %+v, want base index 1 removed", d)
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

// A rename keeps its position (same oid): the new name is an added element
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
	_, err := encodeDelta(base, cur)
	if !errors.Is(err, errNotEncodable) || !strings.Contains(err.Error(), "idx_scan") {
		t.Fatalf("err = %v, want errNotEncodable naming the lost field", err)
	}
}

// Empty current list: every base element is removed and the count is 0.
func TestEncodeDelta_EmptyCurrentRemovesEverything(t *testing.T) {
	d, _ := mustDelta(t, list(idx("a", 1), idx("b", 2)), []byte(`[]`))
	if d.N != 0 || !reflect.DeepEqual(d.D, []int{0, 1}) || len(d.A) != 0 {
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

// Invalid input: only an array of objects with unique identities that are
// present can be delta encoded; anything else is stored in full.
func TestParseCatalog_RejectsWhatADeltaCannotExpress(t *testing.T) {
	cases := map[string]string{
		"null":          `null`,
		"object":        `{"schemaname":"app"}`,
		"scalar":        `42`,
		"non-object":    `[1,2]`,
		"null element":  `[null]`,
		"duplicate key": `[` + idx("a", 1) + `,` + idx("a", 2) + `]`,
		"missing field": `[{"schemaname":"app"}]`,
	}
	for name, doc := range cases {
		if _, err := parseCatalog([]byte(doc), indexFields); !errors.Is(err, errNotEncodable) {
			t.Errorf("%s: err = %v, want errNotEncodable", name, err)
		}
	}
	if _, err := parseCatalog([]byte(`[{"schemaname":`), indexFields); err == nil ||
		errors.Is(err, errNotEncodable) {
		t.Errorf("malformed JSON: err = %v, want a parse error", err)
	}
}

// An identity is the raw JSON of its fields: any JSON type pairs elements,
// integers keep full precision (2^53+1 is not 2^53), and fields join with a
// byte raw JSON cannot hold, so ("a", "bc") and ("ab", "c") differ.
func TestParseCatalog_IdentityIsRawJSON(t *testing.T) {
	c, err := parseCatalog([]byte(`[{"queryid":9007199254740993},`+
		`{"queryid":9007199254740992},{"queryid":-5},{"queryid":null},{"queryid":"7"}]`),
		[]string{"queryid"})
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	want := []string{"9007199254740993", "9007199254740992", "-5", "null", `"7"`}
	if !reflect.DeepEqual(c.keys, want) {
		t.Fatalf("keys = %q, want %q", c.keys, want)
	}
	c, err = parseCatalog([]byte(`[{"schemaname":"a","indexrelname":"bc"},`+
		`{"schemaname":"ab","indexrelname":"c"}]`), indexFields)
	if err != nil || len(c.keys) != 2 || c.keys[0] == c.keys[1] {
		t.Fatalf("keys = %q (%v), want two distinct identities", c.keys, err)
	}
}

// Every delta category pairs its elements by one to three fields, and the
// set of delta categories is the catalog lists.
func TestKeyFields_EveryCatalogCategory(t *testing.T) {
	want := []string{"foreign_keys", "indexes", "io", "partitions", "queries", "sequences",
		"tables"}
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

// An object document (config_data) is a list of one: only the members that
// moved are carried; the static ones (pg_settings) are not repeated.
func TestEncodeDelta_ObjectDocument(t *testing.T) {
	settings := `[{"name":"work_mem","setting":"4096"},{"name":"shared_buffers","setting":"16384"}]`
	doc := func(wal string, churn int) []byte {
		return []byte(fmt.Sprintf(`{"pg_settings":%s,"wal_position":%q,"connection_churn":%d}`,
			settings, wal, churn))
	}
	base, ok, err := parseDocument("config_data", doc("0/1", 3))
	if err != nil || !ok || !base.object {
		t.Fatalf("parse = %+v, %v, %v", base, ok, err)
	}
	cur, _, _ := parseDocument("config_data", doc("0/2", 5))
	raw, err := encodeDelta(base, cur)
	if err != nil {
		t.Fatalf("encodeDelta: %v", err)
	}
	want := `{"w":true,"n":1,"g":{"connection_churn":2},"u":{"0":{"wal_position":"0/2"}}}`
	if string(raw) != want {
		t.Fatalf("delta = %s\nwant    %s", raw, want)
	}
}

// Only catalog lists and object categories are delta encoded; a document
// of the wrong shape for its category is stored in full.
func TestParseDocument_Categories(t *testing.T) {
	if _, ok, err := parseDocument("system", []byte(`{"a":1}`)); ok || err != nil {
		t.Fatalf("system: ok=%v err=%v, want stored in full", ok, err)
	}
	for _, doc := range []string{`null`, `[{"a":1}]`, `7`} {
		if _, ok, err := parseDocument("config_data", []byte(doc)); !ok ||
			!errors.Is(err, errNotEncodable) {
			t.Errorf("config_data %s: ok=%v err=%v, want errNotEncodable", doc, ok, err)
		}
	}
	if _, _, err := parseDocument("config_data", []byte(`{"a":`)); err == nil ||
		errors.Is(err, errNotEncodable) {
		t.Fatalf("malformed object: err = %v, want a parse error", err)
	}
	list, _, _ := parseDocument("indexes", []byte(`[]`))
	obj, _, _ := parseDocument("config_data", []byte(`{}`))
	if _, err := encodeDelta(obj, list); !errors.Is(err, errNotEncodable) {
		t.Fatalf("mixed shapes: err = %v, want errNotEncodable", err)
	}
}
