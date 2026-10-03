package llm

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sanitizeJSONStrings decodes JSON text and runs every string value (not
// object keys, which PostgreSQL defines in plan output) through the SQL
// sanitizer, then re-encodes it compactly. It reports false when text is
// not exactly one JSON value.
func sanitizeJSONStrings(text string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || dec.More() {
		return "", false
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keep <, > and & readable in plan conditions
	if err := enc.Encode(sanitizeJSONValue(value)); err != nil {
		return "", false
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}

func sanitizeJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		return RedactSQLLiterals(StripSQLComments(v))
	case []any:
		for i := range v {
			v[i] = sanitizeJSONValue(v[i])
		}
		return v
	case map[string]any:
		for key, item := range v {
			v[key] = sanitizeJSONValue(item)
		}
		return v
	}
	return value
}
