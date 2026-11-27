package llm

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sanitizeJSONStrings decodes JSON text and runs every string value (not
// object keys, which PostgreSQL defines in plan output) through the SQL
// sanitizer, then re-encodes it compactly. It reports false when text is
// not exactly one JSON value. With numbers, long compared numbers inside
// string values are redacted too; JSON numbers are never touched.
func sanitizeJSONStrings(text string, numbers bool) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || dec.More() {
		return "", false
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keep <, > and & readable in plan conditions
	if err := enc.Encode(sanitizeJSONValue(value, numbers)); err != nil {
		return "", false
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}

func sanitizeJSONValue(value any, numbers bool) any {
	switch v := value.(type) {
	case string:
		return sanitizeSQL(v, true, numbers)
	case []any:
		for i := range v {
			v[i] = sanitizeJSONValue(v[i], numbers)
		}
		return v
	case map[string]any:
		for key, item := range v {
			v[key] = sanitizeJSONValue(item, numbers)
		}
		return v
	}
	return value
}
