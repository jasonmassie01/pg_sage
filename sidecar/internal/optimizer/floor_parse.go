package optimizer

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/workload"
)

// floorIdentPat is one identifier: quoted (its name, "" for a quote) or
// plain (folded to lower case, as PostgreSQL does).
const floorIdentPat = `(?:"((?:[^"]|"")+)"|([A-Za-z_][A-Za-z0-9_$]*))`

// floorColPat is a column, optionally qualified by a table or alias:
// groups 1-2 are the qualifier, 3-4 the column.
const floorColPat = `(?:` + floorIdentPat + `\s*\.\s*)?` + floorIdentPat

var (
	floorLeadRe   = regexp.MustCompile(`(?i)^\s*(SELECT|UPDATE|DELETE)\b`)
	floorWhereRe  = regexp.MustCompile(`(?i)\bWHERE\b`)
	floorSelectRe = regexp.MustCompile(`(?i)\bSELECT\b`)
	floorFromRe   = regexp.MustCompile(`(?i)\bFROM\b`)
	floorSetRe    = regexp.MustCompile(`(?i)\bSET\b`)
	floorUpdateRe = regexp.MustCompile(`(?i)^\s*UPDATE\b`)
	floorOrRe     = regexp.MustCompile(`(?i)\bOR\b`)
	floorMultiRe  = regexp.MustCompile(`(?i),|\bJOIN\b|\bUSING\b`)
	floorEndRe    = regexp.MustCompile(`(?i)\b(?:GROUP\s+BY|ORDER\s+BY|LIMIT|OFFSET|HAVING|` +
		`RETURNING|WINDOW|FETCH|FOR\s+(?:UPDATE|SHARE|NO\s+KEY|KEY))\b`)
	floorEqRe    = regexp.MustCompile(floorColPat + `\s*=\s*\$\d+`)
	floorRangeRe = regexp.MustCompile(`(?i)` + floorColPat +
		`(?:\s*(?:<=|>=|<|>)\s*\$\d+|\s+BETWEEN\s+\$\d+)`)
	floorAliasRe = regexp.MustCompile(`(?i)^\s+(?:AS\s+)?` + floorIdentPat)
)

// floorNotAlias are the words that can follow a relation but name no alias.
var floorNotAlias = map[string]bool{"on": true, "join": true, "inner": true, "left": true,
	"right": true, "full": true, "cross": true, "natural": true, "where": true,
	"using": true, "lateral": true, "outer": true, "tablesample": true, "set": true}

// floorFilter is one statement's filter columns: equality, then range.
type floorFilter struct {
	eq, rng []string
	calls   int64
}

// parseFloorFilters reads the parameterized equality and range filters
// on table's columns in a statement's WHERE clause. Only a plain SELECT,
// UPDATE or DELETE with one WHERE clause, no subquery, no OR and balanced
// parentheses counts; anything else contributes no filter (ok false).
// Literals and comments are blanked first, so neither reads as a filter.
// A statement over several relations (a join, a FROM list, USING) counts
// only the columns qualified by table or one of its aliases, so another
// relation's filter is never read as one of table's.
func parseFloorFilters(text, table string) (floorFilter, bool) {
	blank := workload.BlankLiteralsAndComments(text)
	lead := floorLeadRe.FindStringSubmatch(blank)
	if lead == nil || !parensBalanced(blank) {
		return floorFilter{}, false
	}
	selects := 0
	if strings.EqualFold(lead[1], "SELECT") {
		selects = 1
	}
	wheres := floorWhereRe.FindAllStringIndex(blank, -1)
	if len(wheres) != 1 || len(floorSelectRe.FindAllStringIndex(blank, -1)) != selects {
		return floorFilter{}, false
	}
	seg := blank[wheres[0][1]:]
	if loc := floorEndRe.FindStringIndex(seg); loc != nil {
		seg = seg[:loc[0]]
	}
	if floorOrRe.MatchString(seg) {
		return floorFilter{}, false
	}
	owns := floorOwner(blank[:wheres[0][0]], table)
	var f floorFilter
	f.eq = floorColumns(floorEqRe, seg, owns, nil)
	sort.Strings(f.eq)
	f.rng = floorColumns(floorRangeRe, seg, owns, f.eq)
	return f, len(f.eq)+len(f.rng) > 0
}

// floorColumns are the distinct columns re matches in seg that owns
// accepts, in order, leaving out those in except.
func floorColumns(re *regexp.Regexp, seg string, owns func(string) bool,
	except []string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(seg, -1) {
		qual, col := floorName(m[1], m[2]), floorName(m[3], m[4])
		if col == "" || !owns(qual) || slices.Contains(except, col) ||
			slices.Contains(out, col) {
			continue
		}
		out = append(out, col)
	}
	return out
}

// floorName is a matched identifier's name: quoted as written, plain
// folded to lower case.
func floorName(quoted, plain string) string {
	if quoted != "" {
		return strings.ReplaceAll(quoted, `""`, `"`)
	}
	return strings.ToLower(plain)
}

// floorOwner tells whether a filter qualified by qual (empty: none) is on
// table, from the statement's text before WHERE: in a single-relation
// statement every filter is; in one over several relations only those
// qualified by the table's name or one of its aliases.
func floorOwner(head, table string) func(string) bool {
	if !floorMultiRe.MatchString(floorRelations(head)) {
		return func(string) bool { return true }
	}
	names := map[string]bool{strings.ToLower(table): true}
	for _, a := range floorAliases(head, table) {
		names[strings.ToLower(a)] = true
	}
	return func(qual string) bool { return qual != "" && names[strings.ToLower(qual)] }
}

// floorRelations is the relation list of a statement's text before WHERE:
// an UPDATE's target plus what its FROM adds (never its SET list), else
// what follows the first FROM outside parentheses (the whole head when
// there is none).
func floorRelations(head string) string {
	from := floorTopFrom(head)
	if loc := floorSetRe.FindStringIndex(head); loc != nil && floorUpdateRe.MatchString(head) {
		if from < loc[1] {
			return head[:loc[0]]
		}
		return head[:loc[0]] + ", " + head[from:]
	}
	if from < 0 {
		return head
	}
	return head[from:]
}

// floorTopFrom is where the first FROM outside parentheses ends, -1 when
// there is none.
func floorTopFrom(head string) int {
	for _, loc := range floorFromRe.FindAllStringIndex(head, -1) {
		if strings.Count(head[:loc[0]], "(") == strings.Count(head[:loc[0]], ")") {
			return loc[1]
		}
	}
	return -1
}

// floorAliases are the aliases text gives table ("orders o", "orders AS
// o", quoted or schema-qualified).
func floorAliases(text, table string) []string {
	ref := regexp.MustCompile(`(?i)(?:^|[\s,(.])(?:"` + regexp.QuoteMeta(table) + `"|` +
		regexp.QuoteMeta(table) + `\b)`)
	var out []string
	for _, loc := range ref.FindAllStringIndex(text, -1) {
		m := floorAliasRe.FindStringSubmatch(text[loc[1]:])
		if m == nil {
			continue
		}
		if a := floorName(m[1], m[2]); m[1] != "" || !floorNotAlias[a] {
			out = append(out, a)
		}
	}
	return out
}

func parensBalanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}
