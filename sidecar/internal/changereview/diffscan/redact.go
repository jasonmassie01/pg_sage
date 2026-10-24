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
	awsKeyID = regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`)
	// urlCreds is scheme://user:password@ up to the last '@' of the URL: a
	// password may hold '/' or an unencoded '@'.
	urlCreds  = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s:@'"\[\]]+:[^\s'"]+@`)
	secretKey = `[A-Za-z0-9_\-]*(?:password|passwd|pwd|secret|token|api_?key|access_?key|` +
		`private_?key|auth_?key|credentials?)[A-Za-z0-9_\-]*`
	assignment = regexp.MustCompile(`(?i)\b` + secretKey +
		`["']?\s*(?:=>|:=|=|:)\s*("[^"\n]*"|'[^'\n]*'|[^\s,;'"]+)`)
	// sqlSecret is SQL's keyword form: PASSWORD 'x', OPTIONS (password 'x').
	sqlSecret = regexp.MustCompile(`(?i)\b(` + secretKey + `\s+)('(?:[^'\n]|'')*')`)
	longToken = regexp.MustCompile(`[A-Za-z0-9+_\-]{32,}={0,2}`)
	// Unquoted values that are code, not a secret: a variable or template
	// reference, a member path, a call or an index.
	envRef     = regexp.MustCompile(`^\$(?:\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*|\()`)
	memberPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	callOrItem = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*[(\[]`)
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
	text = sqlSecret.ReplaceAllStringFunc(text, redactSQLSecret)
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
	if codeValue(value) || notSecretValues[strings.ToLower(value)] || value == Redacted {
		return m
	}
	return key + Redacted
}

// codeValue reports whether an unquoted assignment value is code that
// names a secret rather than the secret: a variable or template reference
// ($VAR, ${VAR}, <%= %>, {{ }}), a member path (settings.DB_PASSWORD) or
// a call or index (ENV.fetch(, params[). A value with other punctuation
// (s3cr3t.2024, Hunter2$, p@ss.word!) is a literal.
func codeValue(v string) bool {
	return envRef.MatchString(v) || strings.HasPrefix(v, "<%") ||
		strings.HasPrefix(v, "{{") || memberPath.MatchString(v) || callOrItem.MatchString(v)
}

// redactSQLSecret replaces the quoted literal of PASSWORD 'x'.
func redactSQLSecret(m string) string {
	sub := sqlSecret.FindStringSubmatch(m)
	if sub == nil || sub[2] == "''" || sub[2] == "'"+Redacted+"'" {
		return m
	}
	return sub[1] + "'" + Redacted + "'"
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
