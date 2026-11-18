package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// FieldDiff is one field where an exception's document differs from its
// set's: Field is a dotted path, and a side without the field is null.
type FieldDiff struct {
	Field     string          `json:"field"`
	Set       json.RawMessage `json:"set"`
	Exception json.RawMessage `json:"exception"`
}

// DocumentDiff lists the fields where exception differs from set, sorted
// by path. Objects are compared field by field; anything else as a whole.
func DocumentDiff(set, exception json.RawMessage) ([]FieldDiff, error) {
	var a, b any
	if err := json.Unmarshal(set, &a); err != nil {
		return nil, fmt.Errorf("decode the set's document: %w", err)
	}
	if err := json.Unmarshal(exception, &b); err != nil {
		return nil, fmt.Errorf("decode the exception's document: %w", err)
	}
	out := make([]FieldDiff, 0)
	diffValues("", a, b, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out, nil
}

func diffValues(path string, a, b any, out *[]FieldDiff) {
	am, aObject := a.(map[string]any)
	bm, bObject := b.(map[string]any)
	if aObject && bObject {
		keys := make(map[string]bool, len(am)+len(bm))
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			diffValues(joinPath(path, k), am[k], bm[k], out)
		}
		return
	}
	left, right := canonicalJSON(a), canonicalJSON(b)
	if !bytes.Equal(left, right) {
		*out = append(*out, FieldDiff{Field: path, Set: left, Exception: right})
	}
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// canonicalJSON re-encodes a decoded value (maps encode with sorted keys).
func canonicalJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null") // decoded JSON always re-encodes
	}
	return raw
}
