package sre

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// Redaction (CHECK-30): evidence, hypotheses and exports carry untrusted
// text (application names, identifiers, probe errors). Before any of it
// leaves the store it is stripped of connection URIs, credentials,
// bearer tokens, SQL string literals and raw vectors. Numbers and
// structure are kept.

// RedactionRules names what redaction removes, for the export header.
var RedactionRules = []string{"connection_uri", "credential", "bearer_token",
	"sql_literal", "raw_vector"}

var redactions = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]*@[^\s"'<>]*`),
		"[redacted connection_uri]"},
	{regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis)://` +
		`[^\s"'<>]*`), "[redacted connection_uri]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|` +
		`sslpassword|sslkey)(\s*[=:]\s*)('[^']*'|"[^"]*"|[^\s,;"']+)`),
		"${1}${2}[redacted credential]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`), "Bearer [redacted bearer_token]"},
	{regexp.MustCompile(`\[\s*-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?` +
		`(?:\s*,\s*-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?){7,}\s*\]`), "[redacted raw_vector]"},
	{regexp.MustCompile(`(?i)(=|<>|!=|<|>|\bIN\s*\(|\bLIKE|\bVALUES\s*\(|,|\()` +
		`(\s*)'(?:[^']|'')*'`), "${1}${2}'[redacted sql_literal]'"},
}

// RedactText removes credentials, connection URIs, bearer tokens, SQL
// literals and raw vectors from untrusted text.
func RedactText(s string) string {
	for _, r := range redactions {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}

// redactJSON redacts every string value (and key) of a JSON document,
// keeping numbers exact.
func redactJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(redactValue(v))
}

func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return RedactText(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[RedactText(k)] = redactValue(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
		return x
	default:
		return v
	}
}

// redactInto redacts a value through its JSON form into out.
func redactInto(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	clean, err := redactJSON(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(clean))
	dec.UseNumber()
	return dec.Decode(out)
}
