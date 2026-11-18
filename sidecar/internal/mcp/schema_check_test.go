package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// A structural JSON Schema 2020-12 check for the keywords pg_sage's tool
// schemas use, and a validator for that subset (mcp-01a). The module has
// no JSON Schema library, and none may be added offline.

// schemaKeywords are the 2020-12 keywords the tool schemas may use.
var schemaKeywords = map[string]bool{"type": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true, "enum": true, "const": true,
	"description": true, "title": true, "examples": true, "oneOf": true, "anyOf": true,
	"pattern": true, "format": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "minItems": true, "maxItems": true, "default": true,
	"uniqueItems": true}

var schemaTypes = map[string]bool{"object": true, "string": true, "integer": true,
	"number": true, "boolean": true, "array": true, "null": true}

// checkSchema reports the first structural error in schema, by path.
func checkSchema(path string, schema map[string]any) error {
	for _, key := range sortedKeys(schema) {
		if !schemaKeywords[key] {
			return fmt.Errorf("%s: unknown keyword %q", path, key)
		}
	}
	if err := checkSchemaType(path, schema["type"]); err != nil {
		return err
	}
	if err := checkSchemaValues(path, schema); err != nil {
		return err
	}
	properties, _ := schema["properties"].(map[string]any)
	if raw, ok := schema["required"]; ok {
		names, isList := raw.([]any)
		if !isList {
			return fmt.Errorf("%s: required is not an array", path)
		}
		for _, name := range names {
			if _, declared := properties[fmt.Sprint(name)]; !declared {
				return fmt.Errorf("%s: required %v is not a declared property", path, name)
			}
		}
	}
	return checkSubschemas(path, schema, properties)
}

func checkSchemaType(path string, raw any) error {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		if !schemaTypes[value] {
			return fmt.Errorf("%s: unknown type %q", path, value)
		}
	case []any:
		for _, each := range value {
			if err := checkSchemaType(path, each); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: type is %T", path, raw)
	}
	return nil
}

func checkSchemaValues(path string, schema map[string]any) error {
	if raw, ok := schema["enum"]; ok {
		if values, isList := raw.([]any); !isList || len(values) == 0 {
			return fmt.Errorf("%s: enum must be a non-empty array", path)
		}
	}
	if raw, ok := schema["pattern"]; ok {
		if _, err := regexp.Compile(fmt.Sprint(raw)); err != nil {
			return fmt.Errorf("%s: pattern %v does not compile: %v", path, raw, err)
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "examples"} {
		if raw, ok := schema[key]; ok {
			if values, isList := raw.([]any); !isList || len(values) == 0 {
				return fmt.Errorf("%s: %s must be a non-empty array", path, key)
			}
		}
	}
	return nil
}

func checkSubschemas(path string, schema, properties map[string]any) error {
	for _, name := range sortedKeys(properties) {
		child, ok := properties[name].(map[string]any)
		if !ok {
			return fmt.Errorf("%s.%s: property schema is not an object", path, name)
		}
		if err := checkSchema(path+"."+name, child); err != nil {
			return err
		}
	}
	var children []map[string]any
	if items, ok := schema["items"].(map[string]any); ok {
		children = append(children, items)
	}
	if extra, ok := schema["additionalProperties"].(map[string]any); ok {
		children = append(children, extra)
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		branches, _ := schema[key].([]any)
		for _, branch := range branches {
			child, ok := branch.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s: branch is not an object", path, key)
			}
			children = append(children, child)
		}
	}
	for _, child := range children {
		if err := checkSchema(path+"[]", child); err != nil {
			return err
		}
	}
	return nil
}

// matchesSchema validates value against the subset: type, const, enum,
// properties, required and additionalProperties false.
func matchesSchema(schema map[string]any, value any) bool {
	if want, ok := schema["const"]; ok && fmt.Sprint(want) != fmt.Sprint(value) {
		return false
	}
	if values, ok := schema["enum"].([]any); ok && !containsValue(values, value) {
		return false
	}
	if !matchesType(fmt.Sprint(schema["type"]), value) {
		return false
	}
	object, isObject := value.(map[string]any)
	if !isObject {
		return true
	}
	properties, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	for _, name := range required {
		if _, ok := object[fmt.Sprint(name)]; !ok {
			return false
		}
	}
	for name, field := range object {
		child, declared := properties[name].(map[string]any)
		if !declared && schema["additionalProperties"] == false {
			return false
		}
		if declared && !matchesSchema(child, field) {
			return false
		}
	}
	return true
}

func matchesType(want string, value any) bool {
	switch want {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer", "number":
		_, ok := value.(json.Number)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	}
	return true
}

func containsValue(values []any, value any) bool {
	for _, each := range values {
		if fmt.Sprint(each) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

// matchingBranches are the indexes of the oneOf branches value matches.
func matchingBranches(branches []any, value any) []int {
	var out []int
	for i, branch := range branches {
		if child, ok := branch.(map[string]any); ok && matchesSchema(child, value) {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}
