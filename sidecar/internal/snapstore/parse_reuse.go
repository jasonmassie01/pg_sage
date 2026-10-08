package snapstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/maphash"
)

// textSeed hashes element texts for reuse lookups within this process.
var textSeed = maphash.MakeSeed()

// parseCatalog parses a category document as a list of uniquely keyed
// objects. An element whose text equals one in prev (the previous document
// of the category) shares prev's parse and identity: most elements of a
// catalog document are identical cycle to cycle, and parsed elements are
// never modified. Malformed JSON is an error; a valid document a delta
// cannot express (not an array, not objects, a missing or duplicate
// identity) is errNotEncodable.
func parseCatalog(data []byte, fields []string, prev *catalog) (*catalog, error) {
	var raw []json.RawMessage
	if err := unmarshalDocument(data, &raw); err != nil {
		return nil, err
	}
	if bytes.TrimSpace(data)[0] != '[' {
		return nil, fmt.Errorf("%w: not an array", errNotEncodable)
	}
	c := &catalog{keys: make([]string, len(raw)), pos: make(map[string]int, len(raw)),
		items: make([]map[string]json.RawMessage, len(raw)), raw: raw,
		byText: make(map[uint64][]int, len(raw))}
	for i, text := range raw {
		h := maphash.Bytes(textSeed, text)
		c.byText[h] = append(c.byText[h], i)
		item, k := prev.reuse(h, text)
		if item == nil {
			var err error
			if item, k, err = parseElement(i, text, fields); err != nil {
				return nil, err
			}
		}
		if _, dup := c.pos[k]; dup {
			return nil, fmt.Errorf("%w: duplicate identity %q", errNotEncodable, k)
		}
		c.items[i], c.keys[i], c.pos[k] = item, k, i
	}
	return c, nil
}

// reuse returns c's parse and identity of an element with exactly text (h
// its hash), nil when c has none.
func (c *catalog) reuse(h uint64, text []byte) (map[string]json.RawMessage, string) {
	if c == nil || c.byText == nil {
		return nil, ""
	}
	for _, j := range c.byText[h] {
		if bytes.Equal(c.raw[j], text) {
			return c.items[j], c.keys[j]
		}
	}
	return nil, ""
}

// parseElement decodes element i of a catalog document and its identity.
func parseElement(i int, text []byte, fields []string) (map[string]json.RawMessage, string,
	error) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(text, &item); err != nil {
		return nil, "", fmt.Errorf("%w: element %d: %w", errNotEncodable, i, err)
	}
	if item == nil {
		return nil, "", fmt.Errorf("%w: element %d is null", errNotEncodable, i)
	}
	k, err := elementKey(item, fields)
	return item, k, err
}
