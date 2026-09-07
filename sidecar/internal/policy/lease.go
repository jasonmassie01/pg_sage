package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type LeaseID string

type TargetObject struct {
	Schema    string
	Name      string
	Canonical string
}

type LeaseManager interface {
	AcquireLease(context.Context, string, []TargetObject, string) (LeaseID, error)
	ReleaseLease(context.Context, LeaseID) error
}

func NormalizeTargetObjects(inputs []string) ([]TargetObject, error) {
	unique := make(map[string]TargetObject, len(inputs))
	for _, input := range inputs {
		object, err := normalizeTargetObject(input)
		if err != nil {
			return nil, err
		}
		unique[object.Canonical] = object
	}
	objects := make([]TargetObject, 0, len(unique))
	for _, object := range unique {
		objects = append(objects, object)
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Canonical < objects[j].Canonical
	})
	return objects, nil
}

func normalizeTargetObject(input string) (TargetObject, error) {
	trimmed := strings.TrimSpace(input)
	parts, err := splitQualifiedIdentifier(trimmed)
	if err != nil || len(parts) != 2 {
		return TargetObject{}, fmt.Errorf("target object %q must be schema-qualified", input)
	}
	schema, schemaCanonical, err := normalizeIdentifier(parts[0])
	if err != nil {
		return TargetObject{}, fmt.Errorf("target object %q: %w", input, err)
	}
	name, nameCanonical, err := normalizeIdentifier(parts[1])
	if err != nil {
		return TargetObject{}, fmt.Errorf("target object %q: %w", input, err)
	}
	return TargetObject{
		Schema: schema, Name: name, Canonical: schemaCanonical + "." + nameCanonical,
	}, nil
}

func splitQualifiedIdentifier(input string) ([]string, error) {
	if input == "" {
		return nil, fmt.Errorf("empty identifier")
	}
	quoted := false
	start := 0
	parts := []string{}
	for index := 0; index < len(input); index++ {
		switch input[index] {
		case '"':
			quoted = !quoted
		case '.':
			if !quoted {
				parts = append(parts, input[start:index])
				start = index + 1
			}
		case ';':
			if !quoted {
				return nil, fmt.Errorf("unsafe delimiter")
			}
		}
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quoted identifier")
	}
	return append(parts, input[start:]), nil
}

func normalizeIdentifier(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if len(input) >= 2 && input[0] == '"' && input[len(input)-1] == '"' {
		name := input[1 : len(input)-1]
		if name == "" || strings.Contains(name, "\"") {
			return "", "", fmt.Errorf("invalid quoted identifier")
		}
		return name, `"` + name + `"`, nil
	}
	if !validUnquotedIdentifier(input) {
		return "", "", fmt.Errorf("invalid identifier %q", input)
	}
	name := strings.ToLower(input)
	return name, name, nil
}

func validUnquotedIdentifier(value string) bool {
	for index, character := range value {
		if index == 0 && character != '_' && !unicode.IsLetter(character) {
			return false
		}
		if index > 0 && character != '_' && character != '$' &&
			!unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return value != ""
}

func LeaseKey(objects []TargetObject) string {
	canonical := make([]string, len(objects))
	for index, object := range objects {
		canonical[index] = object.Canonical
	}
	sort.Strings(canonical)
	digest := sha256.Sum256([]byte(strings.Join(canonical, "\x00")))
	return hex.EncodeToString(digest[:])
}
