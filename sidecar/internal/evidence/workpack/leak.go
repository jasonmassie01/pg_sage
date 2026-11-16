package workpack

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The model tier's leak guard (evid-02c). Before a model tier is returned,
// every string in it (values and map keys) is checked against (a) the
// most-common values and histogram bounds the build read, (b) every string
// literal of the local tier's texts, plans, definitions and notes, and (c)
// credential-shaped tokens. pg_sage has no secret-pattern scanner to reuse:
// internal/secretscan matches known secrets by hash and length only.
//
// A value of (a) or (b) is a needle only when it has at least minNeedle
// characters and a letter, and is not a name of the pack or a word of the
// model tier's own statement structure: short codes, numbers and
// identifiers are everywhere and are not what this guard protects.

const minNeedle = 6

// needle is one value that must not reach the model tier.
type needle struct {
	value, kind string
}

// Needle kinds, as ErrModelLeak names them.
const (
	kindSample  = "a most-common value or histogram bound the build read"
	kindLiteral = "a string literal of the local tier"
)

// credentialPatterns are token shapes that are credentials wherever seen.
var credentialPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"sk-", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)},
	{"AKIA", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"ghp_", regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}`)},
	{"xox", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{8,}`)},
	{"-----BEGIN", regexp.MustCompile(`-----BEGIN [A-Z ]*-----`)},
}

// leakNeedles reads the needles of the local tier. model's statement
// texts give the words that are structure, not literals.
func leakNeedles(local map[string]any, names map[string]bool,
	model map[string]any) map[string][]needle {
	skip := map[string]bool{}
	for n := range names {
		skip[n] = true
	}
	for _, t := range objects(model[fileTexts], "texts") {
		s, _ := t["text"].(string)
		for _, w := range wordsOutsideLiterals(s) {
			skip[w] = true
		}
	}
	idx := map[string][]needle{}
	add := func(v, kind string) {
		if utf8.RuneCountInString(v) < minNeedle || !strings.ContainsFunc(v,
			unicode.IsLetter) || skip[v] || notKnown[v] {
			return
		}
		idx[v[:minNeedle]] = append(idx[v[:minNeedle]], needle{v, kind})
	}
	for _, c := range objects(local[fileStats], "columns") {
		for _, k := range []string{"most_common_vals", "histogram_bounds"} {
			s, _ := c[k].(string)
			for _, v := range pgArrayElements(s) {
				add(v, kindSample)
			}
		}
	}
	for _, s := range literalSources(local) {
		for _, v := range stringLiterals(s) {
			add(v, kindLiteral)
		}
	}
	return idx
}

// literalSources are the local tier's strings that may hold literals.
func literalSources(local map[string]any) []string {
	var out []string
	collect := func(v any) { out = append(out, allStrings(v)...) }
	for _, t := range objects(local[fileTexts], "texts") {
		collect(t["text"])
	}
	for _, p := range objects(local[filePlans], "plans") {
		collect(p["conditions"])
	}
	for _, t := range objects(local[fileSchema], "tables") {
		collect(t["constraints"])
		collect(t["indexes"])
		collect(t["partition_key"])
	}
	for _, x := range objects(local[fileIndexes], "indexes") {
		collect(x["definition"])
	}
	collect(local[fileContext])
	return out
}

// allStrings is every string value under v.
func allStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, allStrings(e)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, allStrings(e)...)
		}
		return out
	}
	return nil
}

// leakGuard is what checkLeaks searches the model tier for: the needles
// and the pack's names.
type leakGuard struct {
	needles map[string][]needle
	names   map[string]bool
}

// vocabularyKeys are fields whose values come from a closed vocabulary
// (pg_sage's own words, pg_settings' sources, plan node fields, catalog
// types and versions), never from table data or statement literals: a
// needle there is a common word the structure spells, as a setting's
// "default" source is, and is not searched for. Credential-shaped tokens
// are refused in them like everywhere else.
var vocabularyKeys = map[string]bool{"format": true, "tier": true, "status": true,
	"scans_status": true, "source": true, "kind": true, "subject_kind": true,
	"method": true, "command": true, "unit": true, "plan": true, "order": true,
	"granularity": true, "type": true, "error_class": true, "timezone": true,
	"pg_version": true, "platform": true, "Node Type": true, "Parent Relationship": true,
	"Join Type": true, "Strategy": true, "Partial Mode": true, "Scan Direction": true,
	"Operation": true, "Command": true}

// checkLeaks walks the model tier, sections and keys in order, and
// returns ErrModelLeak at the first string holding a needle or a
// credential-shaped token. Map keys are structure (session states, option
// names, fact value keys): only credential-shaped tokens are refused there.
func checkLeaks(model map[string]any, g leakGuard) error {
	for _, name := range sortedNames(model) {
		if path, kind, ok := g.find(model[name], "", ""); ok {
			return fmt.Errorf("%w: %s field %s holds %s", ErrModelLeak, name, path, kind)
		}
	}
	return nil
}

// find searches v, the value of field key at path.
func (g leakGuard) find(v any, path, key string) (string, string, bool) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			// A key is reported by where it stands, never by its text.
			if kind, ok := credentialIn(k); ok {
				return strings.TrimPrefix(path+" (a key)", " "), kind, true
			}
			at := strings.TrimPrefix(path+"."+k, ".")
			if p, kind, ok := g.find(x[k], at, k); ok {
				return p, kind, true
			}
		}
	case []any:
		for i, e := range x {
			if p, kind, ok := g.find(e, path+"["+strconv.Itoa(i)+"]", key); ok {
				return p, kind, true
			}
		}
	case string:
		if kind, ok := g.leakIn(x, key); ok {
			return path, kind, true
		}
	}
	return "", "", false
}

// leakIn reports whether s, a value of field key, holds a credential-shaped
// token or, outside the vocabulary fields and the pack's names, a needle.
func (g leakGuard) leakIn(s, key string) (string, bool) {
	if kind, ok := credentialIn(s); ok {
		return kind, true
	}
	if vocabularyKeys[key] || g.names[s] {
		return "", false
	}
	return needleIn(s, g.needles)
}

func credentialIn(s string) (string, bool) {
	for _, p := range credentialPatterns {
		if p.re.MatchString(s) {
			return "a credential-shaped token (" + p.name + ")", true
		}
	}
	return "", false
}

// needleIn finds a needle in s as a whole token: where the needle starts
// or ends with an identifier character, s must not continue the word
// there (is_active does not hold active; 'active' and x.active do).
func needleIn(s string, needles map[string][]needle) (string, bool) {
	for i := 0; i+minNeedle <= len(s); i++ {
		if i > 0 && isIdentByte(s[i-1]) && isIdentByte(s[i]) {
			continue
		}
		for _, n := range needles[s[i:i+minNeedle]] {
			end := i + len(n.value)
			if !strings.HasPrefix(s[i:], n.value) {
				continue
			}
			if end == len(s) || !isIdentByte(s[end-1]) || !isIdentByte(s[end]) {
				return n.kind, true
			}
		}
	}
	return "", false
}
