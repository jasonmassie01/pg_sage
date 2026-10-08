package snapstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// keySeparator joins the raw JSON of an element's identity fields. Raw JSON
// never holds a NUL byte (control characters are escaped inside strings),
// so distinct identities never join to the same key.
const keySeparator = "\x00"

// errNotEncodable marks a document a delta cannot express exactly. Such a
// document is stored in full: it is a decision, not a failure.
var errNotEncodable = errors.New("document cannot be delta encoded")

// keyFields lists, per delta-encoded category, the element fields that
// pair an element with its previous reading. Pairing only decides what a
// delta has to carry: every field that differs is carried, so any pairing
// decodes to the exact document. objectCategories are single objects
// encoded as a list of one; every other category (system, locks,
// replication) is stored in full every cycle.
var keyFields = map[string][]string{
	"tables":       {"schemaname", "relname"},
	"indexes":      {"schemaname", "indexrelname"},
	"sequences":    {"schemaname", "sequencename"},
	"foreign_keys": {"table_name", "constraint_name", "fk_column"},
	"partitions":   {"child_schema", "child_table"},
	"queries":      {"queryid"},
	"io":           {"backend_type", "object", "context"},
}

// objectCategories are object documents that are mostly static members
// (config_data: pg_settings, reloptions, extensions) with a few moving
// ones. system is left out on purpose: it is small, every member moves,
// and the forecaster and verify read it raw over long windows.
var objectCategories = map[string]bool{"config_data": true}

// isPlainInteger reports whether raw is a JSON number written as an
// integer (-?(0|[1-9][0-9]*)): the numbers whose jsonb text (->>) is the
// same digits. The decoder applies increments only to such numbers.
func isPlainInteger(raw []byte) bool {
	if len(raw) > 0 && raw[0] == '-' {
		raw = raw[1:]
	}
	if len(raw) == 0 || (raw[0] == '0' && len(raw) > 1) {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// int64Digits is the longest plain integer that always fits an int64
// (18 digits, plus a sign): the difference or sum of two such numbers fits
// too, so they need no arbitrary precision.
const int64Digits = 18

func smallInteger(raw []byte) (int64, bool) {
	digits := len(raw)
	if digits > 0 && raw[0] == '-' {
		digits--
	}
	if digits > int64Digits {
		return 0, false
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	return n, err == nil
}

// catalog is a parsed catalog list: elements in collector order with
// their identities. object marks an object document held as its only
// element.
type catalog struct {
	keys   []string
	pos    map[string]int
	items  []map[string]json.RawMessage
	object bool
	// raw is each element's text and byText its elements by text hash, so
	// the next document's parse can reuse identical elements
	// (parse_reuse.go).
	raw    []json.RawMessage
	byText map[uint64][]int
}

// parseDocument parses a category document for delta encoding: a keyed
// list (reusing prev's parse of identical elements), or an object document
// as a list of one. ok is false for a category that is always stored in
// full.
func parseDocument(category string, data []byte, prev *catalog) (c *catalog, ok bool,
	err error) {
	if fields, isList := keyFields[category]; isList {
		c, err = parseCatalog(data, fields, prev)
		return c, true, err
	}
	if !objectCategories[category] {
		return nil, false, nil
	}
	c, err = parseObject(data)
	return c, true, err
}

// parseObject parses an object document as a list of one element.
func parseObject(data []byte) (*catalog, error) {
	var item map[string]json.RawMessage
	if err := unmarshalDocument(data, &item); err != nil {
		return nil, err
	}
	if bytes.TrimSpace(data)[0] != '{' {
		return nil, fmt.Errorf("%w: not an object", errNotEncodable)
	}
	return &catalog{keys: []string{""}, pos: map[string]int{"": 0},
		items: []map[string]json.RawMessage{item}, object: true}, nil
}

// unmarshalDocument decodes data into v in one pass (Unmarshal validates
// the whole input first): malformed JSON is an error, valid JSON of
// another shape is errNotEncodable.
func unmarshalDocument(data []byte, v any) error {
	err := json.Unmarshal(data, v)
	var syntax *json.SyntaxError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &syntax):
		return fmt.Errorf("parse snapshot document: malformed JSON: %w", err)
	}
	return fmt.Errorf("%w: %w", errNotEncodable, err)
}

// elementKey joins the raw JSON of item's identity fields.
func elementKey(item map[string]json.RawMessage, fields []string) (string, error) {
	parts := make([]string, len(fields))
	for i, f := range fields {
		raw, ok := item[f]
		if !ok {
			return "", fmt.Errorf("%w: element lacks identity field %s", errNotEncodable, f)
		}
		parts[i] = string(raw)
	}
	return strings.Join(parts, keySeparator), nil
}

// delta is the stored form of a document relative to its base row (a
// keyframe, or a checkpoint delta on one). Elements are addressed by their
// index in the base document:
//
//	w  set when the document is an object, encoded as a list of one
//	n  number of elements of the document
//	g  per field, an increment applied to every base element whose value
//	   is a plain integer (a counter that moves alike everywhere, such as
//	   xid_age, costs one entry instead of one per element)
//	i  per base index, integer fields that differ after g, as increments:
//	   counters move by small, repetitive amounts that compress far better
//	   than their values
//	u  per base index, fields that differ, as values (applied last)
//	a  elements not in the base, in full
//	d  base indices of elements no longer present
//	o  explicit order: base indices, then len(base)+j for a[j]; absent
//	   when the order is base order then a (d is then unused)
type delta struct {
	W bool                                  `json:"w,omitempty"`
	N int                                   `json:"n"`
	G map[string]json.RawMessage            `json:"g,omitempty"`
	I map[string]map[string]json.RawMessage `json:"i,omitempty"`
	U map[string]map[string]json.RawMessage `json:"u,omitempty"`
	A []map[string]json.RawMessage          `json:"a,omitempty"`
	D []int                                 `json:"d,omitempty"`
	O []int                                 `json:"o,omitempty"`
}

// encodeDelta expresses cur relative to base. It fails with
// errNotEncodable when an element lost a field (a patch cannot delete).
func encodeDelta(base, cur *catalog) ([]byte, error) {
	if base.object != cur.object {
		return nil, fmt.Errorf("%w: object and list documents mixed", errNotEncodable)
	}
	d := delta{W: cur.object, N: len(cur.items), G: globalIncrements(base, cur)}
	order := make([]int, 0, len(cur.items))
	for i, k := range cur.keys {
		bi, ok := base.pos[k]
		if !ok {
			order = append(order, len(base.items)+len(d.A))
			d.A = append(d.A, cur.items[i])
			continue
		}
		order = append(order, bi)
		values, incs, err := patchFor(base.items[bi], cur.items[i], d.G)
		if err != nil {
			return nil, fmt.Errorf("element %q: %w", k, err)
		}
		d.U = addPatch(d.U, bi, values)
		d.I = addPatch(d.I, bi, incs)
	}
	if increasing(order) {
		for bi, k := range base.keys {
			if _, kept := cur.pos[k]; !kept {
				d.D = append(d.D, bi)
			}
		}
	} else {
		d.O = order
	}
	return json.Marshal(d)
}

func addPatch(m map[string]map[string]json.RawMessage, bi int,
	patch map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	if len(patch) == 0 {
		return m
	}
	if m == nil {
		m = map[string]map[string]json.RawMessage{}
	}
	m[strconv.Itoa(bi)] = patch
	return m
}

// patchFor returns the fields of cur that differ from base once the
// global increments g are applied: integer fields as increments, every
// other field as its value.
func patchFor(base, cur, g map[string]json.RawMessage) (
	values, incs map[string]json.RawMessage, err error) {
	var lost []string
	for f := range base {
		if _, ok := cur[f]; !ok {
			lost = append(lost, f)
		}
	}
	if len(lost) > 0 {
		sort.Strings(lost)
		return nil, nil, fmt.Errorf("%w: fields removed: %s", errNotEncodable,
			strings.Join(lost, ", "))
	}
	for f, v := range cur {
		b, had := base[f]
		if had && g[f] != nil && isPlainInteger(b) {
			b = add(b, g[f])
		}
		if had && bytes.Equal(b, v) {
			continue
		}
		if inc, ok := increment(b, v); had && ok {
			incs = setField(incs, f, inc)
			continue
		}
		values = setField(values, f, v)
	}
	return values, incs, nil
}

// setField sets m[f], making m on first use: most elements change nothing.
func setField(m map[string]json.RawMessage, f string,
	v json.RawMessage) map[string]json.RawMessage {
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m[f] = v
	return m
}

// increment returns cur - base when both are plain JSON integers.
func increment(base, cur json.RawMessage) (json.RawMessage, bool) {
	if !isPlainInteger(base) || !isPlainInteger(cur) {
		return nil, false
	}
	if bytes.Equal(base, cur) {
		return json.RawMessage("0"), true
	}
	if b, ok := smallInteger(base); ok {
		if c, ok := smallInteger(cur); ok {
			return json.RawMessage(strconv.AppendInt(nil, c-b, 10)), true
		}
	}
	b, _ := new(big.Int).SetString(string(base), 10)
	c, _ := new(big.Int).SetString(string(cur), 10)
	return json.RawMessage(c.Sub(c, b).String()), true
}

// add returns a + b for plain JSON integers.
func add(a, b json.RawMessage) json.RawMessage {
	if x, ok := smallInteger(a); ok {
		if y, ok := smallInteger(b); ok {
			return json.RawMessage(strconv.AppendInt(nil, x+y, 10))
		}
	}
	x, _ := new(big.Int).SetString(string(a), 10)
	y, _ := new(big.Int).SetString(string(b), 10)
	return json.RawMessage(x.Add(x, y).String())
}

// increasing reports whether order is the default order: base elements in
// base order, then the added ones.
func increasing(order []int) bool {
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			return false
		}
	}
	return true
}
