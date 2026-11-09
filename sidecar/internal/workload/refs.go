package workload

import (
	"cmp"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Relation is a table a statement names, by its catalog names (quoted
// identifiers keep their case, bare ones are folded to lower case).
type Relation struct {
	Schema string
	Name   string
}

// KnownTables maps each table name in the snapshot to the schemas holding
// a table of that name, in snapshot order: the catalog ReferencedTables
// resolves statements against.
func KnownTables(snap *collector.Snapshot) map[string][]string {
	out := map[string][]string{}
	if snap == nil {
		return out
	}
	for _, t := range snap.Tables {
		out[t.RelName] = append(out[t.RelName], t.SchemaName)
	}
	return out
}

// ReferencedTables names the known tables a statement reads or writes:
// the relations after FROM, JOIN, UPDATE and INTO (comma lists and ONLY
// included), sorted and without duplicates. A qualified name must be
// known in its schema. An unqualified name resolves to its only schema,
// else to public when public has it: pg_stat_statements' text carries no
// search_path, so a name held by public and another schema is taken as
// public's, and one held only by other schemas is left out. Functions,
// CTE names (an unqualified name a WITH of the statement defines, even one
// named like a table) and unknown relations are left out; string literals
// and comments are never read.
func ReferencedTables(query string, known map[string][]string) []Relation {
	text := BlankLiteralsAndComments(query)
	ctes := cteNames(text)
	var out []Relation
	for i := 0; i < len(text); {
		word, next := nextWord(text, i)
		if word == "" {
			break
		}
		i = next
		switch strings.ToLower(word) {
		case "from", "join", "update", "into":
			var refs []Relation
			refs, i = readRelationList(text, i, known, ctes)
			out = append(out, refs...)
		}
	}
	slices.SortFunc(out, func(a, b Relation) int {
		return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name))
	})
	return slices.Compact(out)
}

// readRelationList reads "rel [alias] {, rel [alias]}" at text[i:]; an
// unqualified name in ctes is a CTE, not a relation.
func readRelationList(text string, i int, known map[string][]string,
	ctes map[string]bool) ([]Relation, int) {
	var out []Relation
	for {
		i = skipSpace(text, i)
		if w, next := wordAt(text, i); strings.EqualFold(w, "only") {
			i = skipSpace(text, next)
		}
		schema, name, next, err := ReadQualified(text, i)
		if err != nil {
			return out, i
		}
		shadowed := schema == "" && ctes[name]
		if ref, ok := resolveRelation(schema, name, known); ok && !shadowed {
			out = append(out, ref)
		}
		i = skipAlias(text, next)
		if i >= len(text) || text[i] != ',' {
			return out, i
		}
		i++
	}
}

// cteNames returns the names the statement's WITH clauses define
// ("WITH [RECURSIVE] name [(columns)] AS [[NOT] MATERIALIZED] (body)" and
// each further ", name ... (body)"). A name counts for the whole
// statement: a table reference outside the CTE's scope that shares its
// name is missed, which leaves the statement out of that table's workload
// rather than adding one that does not read it.
func cteNames(text string) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < len(text); {
		word, next := nextWord(text, i)
		if word == "" {
			break
		}
		i = next
		if !strings.EqualFold(word, "with") {
			continue
		}
		j := skipSpace(text, i)
		if w, n := wordAt(text, j); strings.EqualFold(w, "recursive") {
			j = n
		}
		for {
			name, end, ok := readCTE(text, j)
			if !ok {
				break
			}
			names[name] = true
			if j = skipSpace(text, end); j >= len(text) || text[j] != ',' {
				break
			}
			j++
		}
	}
	return names
}

// readCTE reads "name [(columns)] AS [[NOT] MATERIALIZED] (body)" at
// text[i:] and returns the name and the index after the body.
func readCTE(text string, i int) (string, int, bool) {
	name, j, err := ReadIdent(text, skipSpace(text, i))
	if err != nil {
		return "", i, false
	}
	if j = skipSpace(text, j); j < len(text) && text[j] == '(' {
		j = skipSpace(text, skipParens(text, j))
	}
	w, n := wordAt(text, j)
	if !strings.EqualFold(w, "as") {
		return "", i, false
	}
	j = skipSpace(text, n)
	for _, opt := range []string{"not", "materialized"} {
		if w, n := wordAt(text, j); strings.EqualFold(w, opt) {
			j = skipSpace(text, n)
		}
	}
	if j >= len(text) || text[j] != '(' {
		return "", i, false
	}
	return name, skipParens(text, j), true
}

// skipParens returns the index after the parenthesis that closes the one
// at text[i] (quoted identifiers skipped), or len(text) when none does.
func skipParens(text string, i int) int {
	depth := 0
	for i < len(text) {
		switch text[i] {
		case '"':
			_, next, _ := ReadIdent(text, i)
			i = max(next, i+1)
			continue
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return i
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

// resolveRelation applies ReferencedTables' resolution rule to one name.
func resolveRelation(schema, name string, known map[string][]string) (Relation, bool) {
	schemas := known[name]
	if schema != "" {
		return Relation{Schema: schema, Name: name}, slices.Contains(schemas, schema)
	}
	switch {
	case len(schemas) == 1:
		return Relation{Schema: schemas[0], Name: name}, true
	case slices.Contains(schemas, "public"):
		return Relation{Schema: "public", Name: name}, true
	}
	return Relation{}, false
}

// nextWord returns the next bare word at or after i (skipping anything
// else) and the index after it.
func nextWord(text string, i int) (string, int) {
	for i < len(text) {
		c := text[i]
		if c == '"' {
			_, next, _ := ReadIdent(text, i)
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

// BlankLiteralsAndComments replaces string literals and comments with
// spaces (newlines kept, so the length and line numbers stay) so they
// never read as keywords or relations. Quoted identifiers are kept.
func BlankLiteralsAndComments(q string) string {
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
			_, next, _ := ReadIdent(q, i)
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
