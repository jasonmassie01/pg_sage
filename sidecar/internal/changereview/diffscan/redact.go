package diffscan

import (
	"math"
	"regexp"
	"strings"
)

// Redacted replaces every secret the redactor finds.
const Redacted = "[redacted]"

var (
	pemKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)` +
		`(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	awsKeyID   = regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`)
	urlCreds   = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s:@'"\[\]]+:[^@\s'"/]+@`)
	assignment = regexp.MustCompile(`(?i)\b[A-Za-z0-9_\-]*(?:password|passwd|pwd|secret|` +
		`token|api_?key|access_?key|private_?key|auth_?key|credentials?)[A-Za-z0-9_\-]*` +
		`["']?\s*(?:=>|:=|=|:)\s*("[^"\n]*"|'[^'\n]*'|[^\s,;'"]+)`)
	longToken = regexp.MustCompile(`[A-Za-z0-9+_\-]{32,}={0,2}`)
)

// notSecretValues are unquoted assignment values that name a type or a
// constant, not a secret ("password: string").
var notSecretValues = map[string]bool{"string": true, "str": true, "number": true,
	"int": true, "integer": true, "bool": true, "boolean": true, "text": true, "any": true,
	"null": true, "nil": true, "none": true, "true": true, "false": true,
	"undefined": true, "varchar": true, "bytes": true, "object": true}

// Redact replaces secrets in text with [redacted]: PEM private-key blocks,
// AWS access key ids, scheme://user:pass@ credentials (the user and
// password), the values of password/secret/token/key assignments, and
// high-entropy tokens of 32+ characters. It is deterministic and
// idempotent; code that only names a secret (a column called password, a
// value read from the environment) is kept.
func Redact(text string) string {
	if text == "" {
		return text
	}
	text = pemKey.ReplaceAllString(text, Redacted)
	text = urlCreds.ReplaceAllString(text, "${1}"+Redacted+"@")
	text = awsKeyID.ReplaceAllString(text, Redacted)
	text = assignment.ReplaceAllStringFunc(text, redactAssignment)
	return longToken.ReplaceAllStringFunc(text, func(tok string) string {
		if highEntropy(tok) {
			return Redacted
		}
		return tok
	})
}

// redactAssignment keeps an assignment's key and replaces a literal value.
func redactAssignment(m string) string {
	sub := assignment.FindStringSubmatchIndex(m)
	if sub == nil {
		return m
	}
	value := m[sub[2]:sub[3]]
	key := m[:sub[2]]
	if q := value[0]; q == '"' || q == '\'' {
		inner := value[1 : len(value)-1]
		if inner == "" || inner == Redacted {
			return m
		}
		return key + string(q) + Redacted + string(q)
	}
	if strings.ContainsAny(value, "([{.$<") || notSecretValues[strings.ToLower(value)] ||
		value == Redacted {
		return m
	}
	return key + Redacted
}

// highEntropy reports whether a 32+ character token looks generated: mixed
// case with digits and at least 3.5 bits of entropy a character. Hex
// strings are kept: digests, fingerprints and commit SHAs are pg_sage's
// own references (a hex secret is still caught by its assignment).
func highEntropy(tok string) bool {
	var upper, lower, digit bool
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return upper && lower && digit && entropy(tok) >= 3.5
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}
