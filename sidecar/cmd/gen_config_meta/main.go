// Command gen_config_meta generates sidecar/web/src/generated/config_meta.json
// by reflecting over config.DefaultConfig(). It is a build tool, not part of
// the shipped sidecar binary.
//
// Plan reference: docs/plan_v0.8.5.md §7.3.2.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// fieldMeta is the JSON shape emitted per config key.
// Optional fields use pointer or empty-string elision via `omitempty`
// so the JSON output is stable and small.
type fieldMeta struct {
	Type    string      `json:"type"`
	Default interface{} `json:"default"`
	Doc     string      `json:"doc"`
	Warning string      `json:"warning,omitempty"`
	Mode    string      `json:"mode,omitempty"`
	DocsURL string      `json:"docs_url,omitempty"`
	Secret  bool        `json:"secret,omitempty"`
}

// sensitiveSuffixes lists yaml-key terminal components that require
// a secret:"true" tag (CHECK-T11). Matching is suffix-based so that
// fields like `token_budget_daily` and `context_budget_tokens` — which
// contain the word "token" but are numeric budgets, not auth tokens —
// do NOT trigger the secret-tag requirement.
//
// Plan §7.3.1 regex `(api_key|password|secret|token|tls_(cert|key)|
// encryption_key)` is implemented here as separator-bounded suffix
// matching so "token" only triggers on `_token` (singular, end of
// name) rather than anywhere in the string.
var sensitiveSuffixes = []string{
	"password",
	"api_key",
	"apikey",
	"encryption_key",
	"client_secret",
	"tls_cert",
	"tls_key",
	"secret",
	"token",
}

// isSensitiveName returns true when yamlKey ends in one of the
// sensitive suffixes, separator-bounded. yamlKey is the snake-case
// yaml field name (e.g. "api_key", "token_budget_daily").
func isSensitiveName(yamlKey string) bool {
	k := strings.ToLower(yamlKey)
	for _, s := range sensitiveSuffixes {
		if k == s || strings.HasSuffix(k, "_"+s) {
			return true
		}
	}
	return false
}

const (
	docMinLen = 20
	docMaxLen = 200
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gen_config_meta:", err)
		os.Exit(1)
	}
}

// run parses args, walks config.DefaultConfig(), and writes the
// output file. Returns an error instead of os.Exit so the happy and
// failure paths are unit-testable.
func run(args []string) error {
	fs := flag.NewFlagSet("gen_config_meta", flag.ContinueOnError)
	out := fs.String("out", "web/src/generated/config_meta.json",
		"output path for config_meta.json (relative to sidecar/ or absolute)")
	lifecycleOut := fs.String("lifecycle-out", "",
		"optional output path for the generated field-lifecycle Markdown")
	lifecycleOnly := fs.Bool("lifecycle-only", false,
		"write only the lifecycle Markdown (requires -lifecycle-out)")
	strict := fs.Bool("strict", false,
		"fail if any yaml-tagged field lacks a doc tag (CHECK-T01)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lifecycleOnly {
		if *lifecycleOut == "" {
			return fmt.Errorf("-lifecycle-only requires -lifecycle-out")
		}
		return writeLifecycleReference(*lifecycleOut)
	}

	meta, err := Generate(config.DefaultConfig(), *strict)
	if err != nil {
		return err
	}

	// Sorted JSON for byte-stable output (CHECK-T03).
	buf, err := marshalSorted(meta)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(parentDir(*out), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir(*out), err)
	}
	if err := os.WriteFile(*out, buf, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	if err := writeLifecycleReference(*lifecycleOut); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"gen_config_meta: wrote %d fields to %s\n", len(meta), *out)
	return nil
}

func writeLifecycleReference(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir(path), err)
	}
	if err := os.WriteFile(
		path, []byte(config.ConfigLifecycleMarkdown()), 0o644,
	); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "gen_config_meta: wrote lifecycles to %s\n", path)
	return nil
}

// Generate walks cfg via reflection and returns a keyed map of
// {dot.path: fieldMeta}. When strict is true, any yaml-tagged field
// lacking a doc tag produces an error.
func Generate(cfg *config.Config, strict bool) (map[string]fieldMeta, error) {
	out := make(map[string]fieldMeta)
	v := reflect.ValueOf(cfg).Elem()
	if err := walkStruct(v, "", out, strict); err != nil {
		return nil, err
	}
	return out, nil
}

// walkStruct recursively walks a struct value and populates out.
// prefix is the dot-path accumulated so far.
func walkStruct(
	v reflect.Value, prefix string,
	out map[string]fieldMeta, strict bool,
) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		yamlTag := sf.Tag.Get("yaml")
		if yamlTag == "" || yamlTag == "-" {
			continue
		}
		// yaml:"name,omitempty" → take first token.
		name := strings.SplitN(yamlTag, ",", 2)[0]
		if name == "" {
			continue
		}
		fullKey := name
		if prefix != "" {
			fullKey = prefix + "." + name
		}

		fv := v.Field(i)
		fieldType := fv.Type()

		// Unwrap pointer. nil pointer → skip recursion, emit as type only.
		if fieldType.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
			fieldType = fv.Type()
		}

		switch fieldType.Kind() {
		case reflect.Struct:
			if err := walkStruct(fv, fullKey, out, strict); err != nil {
				return err
			}
			continue
		case reflect.Slice:
			elemType := fieldType.Elem()
			if elemType.Kind() == reflect.Struct {
				// Slice of struct: emit element fields under key[].
				zero := reflect.New(elemType).Elem()
				if err := walkStruct(
					zero, fullKey+"[]", out, strict,
				); err != nil {
					return err
				}
				continue
			}
			// Slice of scalar: emit single entry.
			meta, err := buildFieldMeta(
				sf, "[]"+elemType.Kind().String(), nil, strict, fullKey,
			)
			if err != nil {
				return err
			}
			out[fullKey] = meta
			continue
		case reflect.Map:
			// Map is opaque: key type[key]value, no recursion.
			kind := fmt.Sprintf("map[%s]%s",
				fieldType.Key().Kind(), fieldType.Elem().Kind())
			meta, err := buildFieldMeta(sf, kind, nil, strict, fullKey)
			if err != nil {
				return err
			}
			out[fullKey] = meta
			continue
		}

		// Scalar leaf.
		def := scalarDefault(fv)
		meta, err := buildFieldMeta(
			sf, fieldType.Kind().String(), def, strict, fullKey,
		)
		if err != nil {
			return err
		}
		out[fullKey] = meta
	}
	return nil
}

// buildFieldMeta constructs a fieldMeta from a struct field, applying
// validation (CHECK-T16 doc length, CHECK-T11 secret naming) and
// defaulting rules.
func buildFieldMeta(
	sf reflect.StructField, typeName string,
	def interface{}, strict bool, fullKey string,
) (fieldMeta, error) {
	doc := sf.Tag.Get("doc")
	warning := sf.Tag.Get("warning")
	mode := sf.Tag.Get("mode")
	docsURL := sf.Tag.Get("docs_url")
	secret := sf.Tag.Get("secret") == "true"

	// CHECK-T11: sensitive-name match forces secret=true.
	yamlName := strings.SplitN(sf.Tag.Get("yaml"), ",", 2)[0]
	if isSensitiveName(yamlName) {
		if !secret {
			return fieldMeta{}, fmt.Errorf(
				"field %q matches sensitive-name pattern but has no "+
					"secret:\"true\" tag (CHECK-T11)", fullKey)
		}
	}

	// CHECK-T16: doc length bounds — only enforced when doc is present.
	if doc != "" {
		n := len(doc)
		if n < docMinLen || n > docMaxLen {
			return fieldMeta{}, fmt.Errorf(
				"field %q doc length %d outside [%d,%d] (CHECK-T16)",
				fullKey, n, docMinLen, docMaxLen)
		}
	} else if strict {
		return fieldMeta{}, fmt.Errorf(
			"field %q has no doc tag (CHECK-T01 strict)", fullKey)
	}

	// Secret fields never emit their runtime value.
	if secret {
		def = nil
	}

	return fieldMeta{
		Type:    typeName,
		Default: def,
		Doc:     doc,
		Warning: warning,
		Mode:    mode,
		DocsURL: docsURL,
		Secret:  secret,
	}, nil
}

// scalarDefault extracts the default value for a scalar reflect.Value
// in a form that encodes to JSON without losing type information.
func scalarDefault(fv reflect.Value) interface{} {
	switch fv.Kind() {
	case reflect.Bool:
		return fv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64:
		return fv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64:
		return fv.Uint()
	case reflect.Float32, reflect.Float64:
		return fv.Float()
	case reflect.String:
		return fv.String()
	}
	// Fallback: the empty-interface value the reflect package exposes.
	if fv.CanInterface() {
		return fv.Interface()
	}
	return nil
}

// marshalSorted writes JSON with keys sorted lexicographically so that
// repeated runs produce byte-identical output (CHECK-T03).
func marshalSorted(meta map[string]fieldMeta) ([]byte, error) {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("{\n")
	for i, k := range keys {
		entry, err := json.MarshalIndent(meta[k], "  ", "  ")
		if err != nil {
			return nil, err
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		sb.WriteString("  ")
		sb.Write(keyJSON)
		sb.WriteString(": ")
		sb.Write(entry)
		if i < len(keys)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}\n")
	return []byte(sb.String()), nil
}

func parentDir(p string) string {
	idx := strings.LastIndexAny(p, `/\`)
	if idx < 0 {
		return "."
	}
	return p[:idx]
}
