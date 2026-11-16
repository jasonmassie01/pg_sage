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

// checkLeaks walks the model tier, sections and keys in order, and
// returns ErrModelLeak at the first string holding a needle or a
// credential-shaped token.
func checkLeaks(model map[string]any, needles map[string][]needle) error {
	for _, name := range sortedNames(model) {
		if path, kind, ok := findLeak(model[name], "", needles); ok {
			return fmt.Errorf("%w: %s field %s holds %s", ErrModelLeak, name, path, kind)
		}
	}
	return nil
}

func findLeak(v any, path string, needles map[string][]needle) (string, string, bool) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			at := strings.TrimPrefix(path+"."+k, ".")
			if kind, ok := leakIn(k, needles); ok {
				return at + " (key)", kind, true
			}
			if p, kind, ok := findLeak(x[k], at, needles); ok {
				return p, kind, true
			}
		}
	case []any:
		for i, e := range x {
			if p, kind, ok := findLeak(e, path+"["+strconv.Itoa(i)+"]", needles); ok {
				return p, kind, true
			}
		}
	case string:
		if kind, ok := leakIn(x, needles); ok {
			return path, kind, true
		}
	}
	return "", "", false
}

// leakIn reports whether s holds a needle or a credential-shaped token.
func leakIn(s string, needles map[string][]needle) (string, bool) {
	for _, p := range credentialPatterns {
		if p.re.MatchString(s) {
			return "a credential-shaped token (" + p.name + ")", true
		}
	}
	for i := 0; i+minNeedle <= len(s); i++ {
		for _, n := range needles[s[i:i+minNeedle]] {
			if strings.HasPrefix(s[i:], n.value) {
				return n.kind, true
			}
		}
	}
	return "", false
}
