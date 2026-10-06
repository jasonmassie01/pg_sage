package specialist

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// A small validator for the OpenAPI 3.1 subset the contract uses (type,
// required, properties, additionalProperties: false, $ref, oneOf, enum,
// items, pattern, minLength/maxLength), so a response can be checked
// against the frozen v1 baseline and the current document.

func schemaNamed(doc map[string]any, name string) map[string]any {
	return doc["components"].(map[string]any)["schemas"].(map[string]any)[name].(map[string]any)
}

// validateValue returns every violation of value against schema.
func validateValue(doc, schema map[string]any, value any, path string) []string {
	if ref, ok := schema["$ref"].(string); ok {
		return validateValue(doc, schemaNamed(doc, strings.TrimPrefix(ref,
			"#/components/schemas/")), value, path)
	}
	if alts, ok := schema["oneOf"].([]any); ok {
		matched := 0
		for _, alt := range alts {
			if len(validateValue(doc, alt.(map[string]any), value, path)) == 0 {
				matched++
			}
		}
		if matched != 1 {
			return []string{fmt.Sprintf("%s: %d oneOf alternatives match", path, matched)}
		}
		return nil
	}
	if out := checkType(schema, value, path); len(out) > 0 {
		return out
	}
	out := checkEnum(schema, value, path)
	switch v := value.(type) {
	case map[string]any:
		out = append(out, validateObject(doc, schema, v, path)...)
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range v {
				out = append(out, validateValue(doc, items, item, fmt.Sprintf("%s[%d]",
					path, i))...)
			}
		}
	case string:
		out = append(out, checkString(schema, v, path)...)
	}
	return out
}

func jsonTypeOf(v any) string {
	switch n := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func checkType(schema map[string]any, value any, path string) []string {
	var allowed []string
	switch t := schema["type"].(type) {
	case nil:
		return nil
	case string:
		allowed = []string{t}
	case []any:
		for _, x := range t {
			allowed = append(allowed, x.(string))
		}
	}
	got := jsonTypeOf(value)
	for _, a := range allowed {
		if a == got || (a == "number" && got == "integer") {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: type %s, want %v", path, got, allowed)}
}

func checkEnum(schema map[string]any, value any, path string) []string {
	enum, ok := schema["enum"].([]any)
	if !ok {
		return nil
	}
	for _, e := range enum {
		if reflect.DeepEqual(e, value) {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: %v is not one of %v", path, value, enum)}
}

func checkString(schema map[string]any, v, path string) []string {
	var out []string
	n := float64(utf8.RuneCountInString(v))
	if max, ok := schema["maxLength"].(json.Number); ok {
		if m, _ := max.Float64(); n > m {
			out = append(out, path+": longer than maxLength")
		}
	}
	if min, ok := schema["minLength"].(json.Number); ok {
		if m, _ := min.Float64(); n < m {
			out = append(out, path+": shorter than minLength")
		}
	}
	if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(v) {
		out = append(out, path+": does not match "+p)
	}
	return out
}

func validateObject(doc, schema, v map[string]any, path string) []string {
	var out []string
	props, _ := schema["properties"].(map[string]any)
	req, _ := schema["required"].([]any)
	for _, r := range req {
		if _, ok := v[r.(string)]; !ok {
			out = append(out, path+": missing required "+r.(string))
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, ok := props[k].(map[string]any)
		if !ok {
			if schema["additionalProperties"] == false {
				out = append(out, path+": unknown property "+k)
			}
			continue
		}
		out = append(out, validateValue(doc, p, v[k], path+"."+k)...)
	}
	return out
}

// decodeNumbers parses JSON keeping numbers exact.
func decodeNumbers(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}
