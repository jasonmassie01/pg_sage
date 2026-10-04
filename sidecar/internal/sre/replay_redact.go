package sre

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"regexp"
	"strings"
	"time"
)

// Redaction of a replay-case export (roadmap 2.4). Probe rows carry
// catalog metadata, never table rows, but identifiers (relations, slots,
// databases, roles, application names, client addresses) and free text
// (errors, the incident subject) can carry secrets and personal data.
// Default-deny: a string value is kept only when it is a timestamp, a
// WAL position or an enumerated server value of a known column; every
// other string is replaced by a keyed hash (equal values hash equally
// within one export, so the causal graph's grouping survives, and a
// fresh key per export makes two exports unlinkable). An operator may
// opt in to keeping identifiers; secrets and PII-like literals are
// removed from them even then. Numbers and booleans are kept exactly.

// enumColumns hold server-defined enumerated values (states, lock modes,
// kinds); a value is kept when it also looks like one.
var enumColumns = map[string]bool{"state": true, "waiter_state": true,
	"blocker_state": true, "root_state": true, "lock_type": true, "requested_mode": true,
	"blocker_kind": true, "root_kind": true, "holder_kind": true, "kind": true,
	"slot_type": true, "wal_status": true, "archive_mode": true, "pool_mode": true,
	"receiver_status": true, "wait_event_type": true, "wait_event": true, "outcome": true,
	"action_type": true, "data_type": true, "owner_type": true}

var (
	enumValue  = regexp.MustCompile(`^[A-Za-z][A-Za-z_ ()]{0,40}$`)
	lsnValue   = regexp.MustCompile(`^[0-9A-F]{1,8}/[0-9A-F]{1,8}$`)
	keyPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,63}$`)
	codeValue  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	quotedName = regexp.MustCompile(`"[^"]*"`)
	dottedName = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_$]*\.[A-Za-z_][A-Za-z0-9_$]*\b`)
	queryIDSub = regexp.MustCompile(`^queryid -?\d+$`)
)

// piiPatterns remove PII-like literals and well-known token formats
// that RedactText (credentials, URIs, bearer tokens) does not cover.
var piiPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{6,}|sk-[A-Za-z0-9_-]{16,}|` +
		`gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|` +
		`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[redacted token]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[redacted email]"},
	{regexp.MustCompile(`\+\d{1,3}[ .-]?\(?\d{2,4}\)?(?:[ .-]?\d{2,4}){2,3}`),
		"[redacted phone]"},
	{regexp.MustCompile(`\d{3}-\d{2}-\d{4}`), "[redacted number]"},
	{regexp.MustCompile(`\d(?:[ -]?\d){12,18}`), "[redacted number]"},
}

// scrub removes secrets and PII-like literals from text.
func scrub(s string) string {
	s = RedactText(s)
	for _, p := range piiPatterns {
		s = p.pattern.ReplaceAllString(s, p.replacement)
	}
	return s
}

// redactor redacts one export with one key.
type redactor struct {
	keep bool
	key  []byte
}

// token is the keyed hash of an identifier.
func (r redactor) token(s string) string {
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(s))
	return "id_" + hex.EncodeToString(mac.Sum(nil)[:6])
}

// value redacts one row value under its column name.
func (r redactor) value(column string, v any) any {
	switch x := v.(type) {
	case string:
		return r.str(column, x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = r.value(column, x[i])
		}
		return out
	case map[string]any:
		return r.row(x)
	default: // json.Number, bool, nil
		return v
	}
}

// row redacts a row; a key that is not a plain column name is hashed.
func (r redactor) row(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		key := k
		if !keyPattern.MatchString(k) {
			key = r.token(k)
		}
		out[key] = r.value(k, v)
	}
	return out
}

func (r redactor) str(column, s string) string {
	if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return s
	}
	switch {
	case lsnValue.MatchString(s), enumColumns[column] && enumValue.MatchString(s):
		return s
	case r.keep && net.ParseIP(s) != nil:
		return s
	case r.keep:
		return scrub(s)
	}
	return r.token(s)
}

// text redacts free text (a probe error): secrets and PII always, and
// quoted or schema-qualified names unless identifiers are kept.
func (r redactor) text(s string) string {
	s = scrub(s)
	if r.keep {
		return s
	}
	s = quotedName.ReplaceAllStringFunc(s, func(q string) string {
		return `"` + r.token(strings.Trim(q, `"`)) + `"`
	})
	return dottedName.ReplaceAllStringFunc(s, r.token)
}

// code keeps a machine reason code and redacts anything else as text.
func (r redactor) code(s string) string {
	if codeValue.MatchString(s) {
		return s
	}
	return r.text(s)
}

// subject keeps a plan subject (it names the query the plan family
// diagnoses); any other subject is free text, replaced unless
// identifiers are kept.
func (r redactor) subject(family, s string) string {
	switch {
	case queryIDSub.MatchString(strings.TrimSpace(s)):
		return strings.TrimSpace(s)
	case r.keep:
		return scrub(s)
	}
	return family + " incident (subject redacted)"
}
