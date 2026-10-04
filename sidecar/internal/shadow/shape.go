// Package shadow is pg_sage's shadow mode (roadmap 1.4): below an action
// class's earned trust level, every action pg_sage would have taken is
// recorded as a shadow decision (the exact SQL and rollback, the
// prediction, the evidence and the gate's verdict had the class been
// trusted) and scored later, deterministically, from what actually
// happened: the operator's decision on the same proposal, the same change
// applied by anyone, or a HypoPG what-if for index creates. Scores the
// trust ledger does not already hold as real outcomes count toward
// promotion as shadow evidence. Nothing here executes or locks anything
// on a user object.
package shadow

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// token is one lexical unit of a statement: a word (lower-cased), a
// literal or identifier kept as written, or one punctuation character.
type token struct {
	text  string
	punct bool
}

const punctuation = "(),;=."

var simpleIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// Shape is the normalized shape of a statement, used to match a shadow
// decision to the same change made later by anyone. Comments, spacing,
// case outside literals, needless identifier quoting and a trailing
// semicolon never matter. For an index create the index name,
// CONCURRENTLY and IF NOT EXISTS are dropped, an unqualified table is
// in public and the default access method (btree) is explicit; for an
// index drop CONCURRENTLY, IF EXISTS and CASCADE/RESTRICT are dropped.
func Shape(sql string) string {
	toks := tokenize(sql)
	for len(toks) > 0 && toks[len(toks)-1].text == ";" {
		toks = toks[:len(toks)-1]
	}
	if s, ok := createIndexShape(toks); ok {
		return s
	}
	if s, ok := dropIndexShape(toks); ok {
		return s
	}
	return join(toks)
}

// Fingerprint identifies one decision: its class, target object and
// shape (unambiguous field boundaries), as 64 hex characters.
func Fingerprint(class, object, shape string) string {
	sum := sha256.Sum256([]byte(class + "\x00" + object + "\x00" + shape))
	return hex.EncodeToString(sum[:])
}

// ClassOf is the trust ledger pair of an action type and its SQL: the
// family is empty for a class pg_sage never runs on its own initiative.
func ClassOf(actionType, sql string) (family, class string) {
	req := policy.ActionRequest{SQL: sql}
	if strings.TrimSpace(actionType) != "" {
		req.Contract = &policy.ActionContract{ActionType: actionType}
	}
	c := earned.ClassFor(req)
	return string(earned.SelfFamilyFor(c)), string(c)
}

func tokenize(sql string) []token {
	src := []rune(sql)
	var out []token
	for i := 0; i < len(src); {
		r := src[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case r == '/' && i+1 < len(src) && src[i+1] == '*':
			i = skipBlockComment(src, i+2)
		case r == '\'' || r == '"':
			var text string
			text, i = quoted(src, i)
			out = append(out, token{text: unquoteIdentifier(text)})
		case strings.ContainsRune(punctuation, r):
			out = append(out, token{text: string(r), punct: true})
			i++
		default:
			start := i
			for i < len(src) && !unicode.IsSpace(src[i]) && src[i] != '\'' &&
				src[i] != '"' && !strings.ContainsRune(punctuation, src[i]) {
				i++
			}
			out = append(out, token{text: strings.ToLower(string(src[start:i]))})
		}
	}
	return out
}

func skipBlockComment(src []rune, i int) int {
	for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
		i++
	}
	return min(i+2, len(src))
}

// quoted reads a literal or quoted identifier starting at i (a doubled
// quote is an escaped quote); an unterminated one runs to the end.
func quoted(src []rune, i int) (string, int) {
	q := src[i]
	j := i + 1
	for j < len(src) {
		if src[j] == q {
			if j+1 < len(src) && src[j+1] == q {
				j += 2
				continue
			}
			return string(src[i : j+1]), j + 1
		}
		j++
	}
	return string(src[i:]), len(src)
}

// unquoteIdentifier drops the quotes of an identifier that needs none.
func unquoteIdentifier(text string) string {
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' &&
		simpleIdentifier.MatchString(text[1:len(text)-1]) {
		return text[1 : len(text)-1]
	}
	return text
}

func join(toks []token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && !t.punct && !toks[i-1].punct {
			b.WriteByte(' ')
		}
		b.WriteString(t.text)
	}
	return b.String()
}

// words reports whether toks starts with the given words at i.
func words(toks []token, i int, want ...string) bool {
	if i+len(want) > len(toks) {
		return false
	}
	for k, w := range want {
		if toks[i+k].punct || toks[i+k].text != w {
			return false
		}
	}
	return true
}

// qualifiedName reads a [schema.]name at i, defaulting the schema to
// public; ok is false when there is no name.
func qualifiedName(toks []token, i int) (string, int, bool) {
	if i >= len(toks) || toks[i].punct {
		return "", i, false
	}
	if i+2 < len(toks) && toks[i+1].text == "." && !toks[i+2].punct {
		return toks[i].text + "." + toks[i+2].text, i + 3, true
	}
	return "public." + toks[i].text, i + 1, true
}

func createIndexShape(toks []token) (string, bool) {
	head := "create index"
	i := 2
	switch {
	case words(toks, 0, "create", "index"):
	case words(toks, 0, "create", "unique", "index"):
		head, i = "create unique index", 3
	default:
		return "", false
	}
	for i < len(toks) && !words(toks, i, "on") {
		i++ // CONCURRENTLY, IF NOT EXISTS and the index name never matter
	}
	if words(toks, i+1, "only") {
		i++
	}
	table, i, ok := qualifiedName(toks, i+1)
	if !ok {
		return "", false
	}
	method := "btree"
	if words(toks, i, "using") && i+1 < len(toks) {
		method, i = toks[i+1].text, i+2
	}
	shape := head + " on " + table + " using " + method
	if i < len(toks) && !toks[i].punct {
		shape += " "
	}
	return shape + join(toks[i:]), true
}

func dropIndexShape(toks []token) (string, bool) {
	if !words(toks, 0, "drop", "index") {
		return "", false
	}
	i := 2
	if words(toks, i, "concurrently") {
		i++
	}
	if words(toks, i, "if", "exists") {
		i += 2
	}
	name, _, ok := qualifiedName(toks, i)
	if !ok {
		return "", false
	}
	return "drop index " + name, true
}
