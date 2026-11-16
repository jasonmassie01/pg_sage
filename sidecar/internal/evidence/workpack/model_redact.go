package workpack

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

// The model tier's redaction step, one rule per section. Every other
// section (header, coverage, workload, growth, autovacuum, connections,
// provider) holds numbers, names and fixed words only and passes as is;
// the leak guard (leak.go) checks every section regardless.

// valueRedacted replaces a setting value that may hold a credential.
const valueRedacted = "redacted"

// modelRedact is the redaction step. It is a variable only so a test can
// bypass it and prove the leak guard catches what it would have removed.
var modelRedact = redactForModel

func redactForModel(name string, tree any) any {
	switch name {
	case fileTexts:
		for _, t := range objects(tree, "texts") {
			redactText(t)
		}
	case fileStats:
		dropValueSamples(tree)
	case filePlans:
		for _, p := range objects(tree, "plans") {
			delete(p, "conditions")
		}
	case fileSchema:
		for _, t := range objects(tree, "tables") {
			redactSQLList(t, "constraints")
			redactSQLList(t, "indexes")
			redactSQLField(t, "partition_key")
		}
	case fileIndexes:
		for _, x := range objects(tree, "indexes") {
			redactSQLField(x, "definition")
		}
	case fileContext:
		redactContext(tree)
	case fileSettings:
		for _, s := range objects(tree, "settings") {
			redactSetting(s)
		}
	}
	return tree
}

// redactText sanitizes a statement text with numeric redaction forced on
// and caps it at MaxModelTextChars.
func redactText(t map[string]any) {
	text, _ := t["text"].(string)
	cut, truncated := cutChars(llm.SanitizeForLLMStrict(text), MaxModelTextChars)
	t["text"] = cut
	if truncated {
		t["truncated"] = true
	}
}

// dropValueSamples removes most-common values and histogram bounds.
func dropValueSamples(tree any) {
	if root, ok := tree.(map[string]any); ok {
		root["value_samples"] = false
	}
	for _, c := range objects(tree, "columns") {
		delete(c, "most_common_vals")
		delete(c, "histogram_bounds")
	}
}

func redactSQLField(m map[string]any, key string) {
	if s, ok := m[key].(string); ok {
		m[key] = llm.RedactSQLLiterals(s)
	}
}

func redactSQLList(m map[string]any, key string) {
	list, _ := m[key].([]any)
	for i, e := range list {
		if s, ok := e.(string); ok {
			list[i] = llm.RedactSQLLiterals(s)
		}
	}
}

// redactContext redacts the owner's notes and drops who confirmed them
// (an email address is personal data; when stays).
func redactContext(tree any) {
	root, _ := tree.(map[string]any)
	notes := objects(tree, "facts")
	if brief, ok := root["brief"].(map[string]any); ok {
		notes = append(notes, brief)
	}
	for _, n := range notes {
		delete(n, "confirmed_by")
		values, _ := n["value"].(map[string]any)
		for k, v := range values {
			if s, ok := v.(string); ok {
				values[k], _ = cutChars(llm.RedactSQLLiterals(s), MaxModelNoteChars)
			}
		}
	}
}

// sensitiveSettingWords mark a setting whose value may hold a credential
// or a command line (primary_conninfo, archive_command,
// ssl_passphrase_command, ...).
var sensitiveSettingWords = []string{"conninfo", "password", "passphrase", "command",
	"secret", "token"}

func sensitiveSetting(name string) bool {
	lower := strings.ToLower(name)
	for _, w := range sensitiveSettingWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func redactSetting(s map[string]any) {
	name, _ := s["name"].(string)
	if !sensitiveSetting(name) {
		return
	}
	s["value"] = valueRedacted
	for _, c := range objects(map[string]any{"c": s["changes"]}, "c") {
		c["from"], c["to"] = valueRedacted, valueRedacted
	}
}
