package workpack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// HashIdentifiers (evid-02c): every schema, table, index and column name
// the local tier lists becomes t_<10 hex> of HMAC-SHA256(key, name), in
// structured fields, evidence ids and SQL text alike. Names are compared
// exactly, so two that differ only in case get different pseudonyms; an
// unquoted token of SQL text that names none exactly is folded to lower
// case first, as PostgreSQL reads it. The
// key is per install (LoadOrCreatePseudonymKey), so a name has the same
// pseudonym in every pack and across encryption-key rotations.

// nameKeys hold one name; qualifiedKeys "schema.name" or a bare name.
// Column names also come from schema.json's columns (collectNames).
var (
	nameKeys = map[string]bool{"schema": true, "index": true, "column": true,
		"Schema": true, "Relation Name": true, "Index Name": true}
	qualifiedKeys = map[string]bool{"table": true, "relations": true}
)

// hashSkip are keys whose values are fixed words, times or hashes.
var hashSkip = map[string]bool{"format": true, "status": true, "scans_status": true,
	"plan": true, "order": true, "unit": true, "timezone": true, "granularity": true,
	"fingerprint": true, "captured_at": true, "pg_version": true, "platform": true,
	"method": true, "kind": true, "type": true, "command": true, "source": true,
	"Node Type": true, "error_class": true, "day": true, "queryid": true,
	"subject_kind": true, "confirmed_at": true, "expires_at": true}

// collectNames lists every identifier the local tier's structured fields
// name.
func collectNames(trees map[string]any) map[string]bool {
	names := map[string]bool{}
	add := func(s string, qualified bool) {
		if qualified {
			if schema, rel, ok := strings.Cut(s, "."); ok {
				add2(names, schema)
				add2(names, rel)
				return
			}
		}
		add2(names, s)
	}
	var walk func(v any, key string, subjectKind string)
	walk = func(v any, key, subjectKind string) {
		switch x := v.(type) {
		case map[string]any:
			kind, _ := x["subject_kind"].(string)
			for k, e := range x {
				walk(e, k, kind)
			}
		case []any:
			for _, e := range x {
				walk(e, key, subjectKind)
			}
		case string:
			switch {
			case nameKeys[key]:
				add(x, false)
			case qualifiedKeys[key]:
				add(x, true)
			case key == "subject" && subjectKind != "database":
				add(x, true)
			}
		}
	}
	for _, tree := range trees {
		walk(tree, "", "")
	}
	for _, t := range objects(trees[fileSchema], "tables") {
		for _, c := range objects(t, "columns") {
			if n, ok := c["name"].(string); ok {
				add2(names, n)
			}
		}
	}
	return names
}

func add2(names map[string]bool, s string) {
	if s != "" && s != "*" {
		names[s] = true
	}
}

// pseudonymizer replaces names; out maps each pseudonym it used back.
type pseudonymizer struct {
	key   []byte
	names map[string]bool
	out   map[string]string
}

func newPseudonymizer(key []byte, names map[string]bool) *pseudonymizer {
	return &pseudonymizer{key: key, names: names, out: map[string]string{}}
}

// name is n's pseudonym.
func (ps *pseudonymizer) name(n string) string {
	mac := hmac.New(sha256.New, ps.key)
	mac.Write([]byte(n))
	p := "t_" + hex.EncodeToString(mac.Sum(nil))[:10]
	ps.out[p] = n
	return p
}

// walk replaces names in every string value of v (map keys are fixed
// words and stay).
func (ps *pseudonymizer) walk(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = ps.walk(e, k)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = ps.walk(e, key)
		}
		return x
	case string:
		switch {
		case hashSkip[key]:
			return x
		case key == "id":
			return ps.id(x)
		}
		return ps.text(x)
	}
	return v
}

// id hashes the key part of section/kind/key; the section and kind stay.
func (ps *pseudonymizer) id(s string) string {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 3 {
		return s
	}
	return parts[0] + "/" + parts[1] + "/" + ps.text(parts[2])
}

// text replaces every whole-token name, bare (case-folded) or
// double-quoted (exact).
func (ps *pseudonymizer) text(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '"':
			inner, end := quotedIdent(s, i)
			if ps.names[inner] {
				b.WriteString(ps.name(inner))
			} else {
				b.WriteString(s[i:end])
			}
			i = end
		case isIdentByte(c):
			j := i
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			b.WriteString(ps.bare(s[i:j]))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// bare is an unquoted token's replacement: the name it spells exactly
// (as evidence ids and structured fields write it), else the name it folds
// to, as PostgreSQL folds an unquoted identifier to lower case (FROM
// Public.ORDERS reads public.orders), else the token itself.
func (ps *pseudonymizer) bare(tok string) string {
	if ps.names[tok] {
		return ps.name(tok)
	}
	if folded := foldASCII(tok); ps.names[folded] {
		return ps.name(folded)
	}
	return tok
}

// foldASCII lower-cases A-Z only, as PostgreSQL's downcase_identifier
// does for an unquoted identifier in a multi-byte encoding.
func foldASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// quotedIdent reads the double-quoted identifier at i ("" is one quote):
// its name and the index after it.
func quotedIdent(s string, i int) (string, int) {
	var inner strings.Builder
	j := i + 1
	for j < len(s) {
		if s[j] == '"' {
			if j+1 < len(s) && s[j+1] == '"' {
				inner.WriteByte('"')
				j += 2
				continue
			}
			return inner.String(), j + 1
		}
		inner.WriteByte(s[j])
		j++
	}
	return inner.String(), j
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '$' || c >= 0x80
}
