package tuning

import (
	"slices"
	"strings"
)

// referencedTables names the snapshot tables a statement reads or writes:
// the relations after FROM, JOIN, UPDATE and INTO (comma lists included),
// resolved against the snapshot. An unqualified name resolves to its only
// schema, else to public when public has it; anything else (functions,
// CTE names, unknown relations) is left out.
func referencedTables(query string, known map[string][]string) []string {
	text := blankLiteralsAndComments(query)
	var out []string
	for i := 0; i < len(text); {
		word, next := nextWord(text, i)
		if word == "" {
			break
		}
		i = next
		switch strings.ToLower(word) {
		case "from", "join", "update", "into":
			var refs []string
			refs, i = readRelationList(text, i, known)
			out = append(out, refs...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// readRelationList reads "rel [alias] {, rel [alias]}" at text[i:].
func readRelationList(text string, i int, known map[string][]string) ([]string, int) {
	var out []string
	for {
		i = skipSpace(text, i)
		if w, next := wordAt(text, i); strings.EqualFold(w, "only") {
			i = skipSpace(text, next)
		}
		schema, name, next, err := readQualified(text, i)
		if err != nil {
			return out, i
		}
		if ref := resolveRelation(schema, name, known); ref != "" {
			out = append(out, ref)
		}
		i = skipAlias(text, next)
		if i >= len(text) || text[i] != ',' {
			return out, i
		}
		i++
	}
}

// skipAlias skips an optional "[AS] alias" after a relation.
func skipAlias(text string, i int) int {
	j := skipSpace(text, i)
	w, next := wordAt(text, j)
	if strings.EqualFold(w, "as") {
		j = skipSpace(text, next)
		w, next = wordAt(text, j)
	}
	if w == "" || aliasStop[strings.ToLower(w)] {
		return skipSpace(text, i)
	}
	return skipSpace(text, next)
}

// aliasStop are words that end a relation without an alias.
var aliasStop = map[string]bool{"where": true, "join": true, "inner": true, "left": true,
	"right": true, "full": true, "cross": true, "natural": true, "on": true, "using": true,
	"group": true, "order": true, "limit": true, "offset": true, "set": true,
	"values": true, "select": true, "returning": true, "union": true, "except": true,
	"intersect": true, "having": true, "window": true, "for": true, "default": true,
	"lateral": true, "tablesample": true, "fetch": true}

func resolveRelation(schema, name string, known map[string][]string) string {
	schemas := known[name]
	if schema != "" {
		if slices.Contains(schemas, schema) {
			return qualified(schema, name)
		}
		return ""
	}
	switch {
	case len(schemas) == 1:
		return qualified(schemas[0], name)
	case slices.Contains(schemas, "public"):
		return qualified("public", name)
	}
	return ""
}

// nextWord returns the next bare word at or after i (skipping anything
// else) and the index after it.
func nextWord(text string, i int) (string, int) {
	for i < len(text) {
		c := text[i]
		if c == '"' {
			_, next, _ := readIdent(text, i)
			i = max(next, i+1)
			continue
		}
		if isIdentByte(c, true) {
			j := i
			for j < len(text) && isIdentByte(text[j], j == i) {
				j++
			}
			return text[i:j], j
		}
		i++
	}
	return "", i
}

// wordAt returns the bare word starting exactly at i, if any.
func wordAt(text string, i int) (string, int) {
	if i >= len(text) || !isIdentByte(text[i], true) {
		return "", i
	}
	return nextWord(text, i)
}

// blankLiteralsAndComments replaces string literals and comments with
// spaces so they never read as keywords or relations.
func blankLiteralsAndComments(q string) string {
	b := []byte(q)
	for i := 0; i < len(b); {
		switch {
		case b[i] == '\'':
			i = blankUntil(b, i, i+1, "'")
		case strings.HasPrefix(q[i:], "--"):
			i = blankUntil(b, i, i+2, "\n")
		case strings.HasPrefix(q[i:], "/*"):
			i = blankUntil(b, i, i+2, "*/")
		case b[i] == '"':
			_, next, _ := readIdent(q, i)
			i = max(next, i+1)
		default:
			i++
		}
	}
	return string(b)
}

// blankUntil blanks b[start:] through the first end at or after from and
// returns the index after it.
func blankUntil(b []byte, start, from int, end string) int {
	stop := len(b)
	if j := strings.Index(string(b[from:]), end); j >= 0 {
		stop = from + j + len(end)
	}
	for k := start; k < stop; k++ {
		if b[k] != '\n' {
			b[k] = ' '
		}
	}
	return stop
}
