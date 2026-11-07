package migration

import (
	"regexp"
	"strings"
)

var (
	reAddColumnDef = regexp.MustCompile(`(?i)^ADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?` +
		`(` + identRE + `)\s+(.+)$`)

	// reBuiltinType is a built-in scalar type with optional modifiers,
	// array bounds and collation. A user type could be a domain with a
	// CHECK, which makes ADD COLUMN rewrite the table, so only these pass.
	reBuiltinType = regexp.MustCompile(`(?i)^(?:pg_catalog\.)?(?:smallint|integer|int|` +
		`bigint|int[248]|real|float[48]?|double\s+precision|numeric|decimal|boolean|bool|` +
		`text|varchar|character\s+varying|character|char|bytea|date|timestamptz|` +
		`timestamp|timetz|time|interval|uuid|jsonb|json|inet|cidr|macaddr8?)` +
		`(?:\s*\(\s*\d+\s*(?:,\s*\d+\s*)?\))?(?:\s+with(?:out)?\s+time\s+zone)?` +
		`(?:\s*\[\s*\d*\s*\])*(?:\s+COLLATE\s+` + qualifiedRE + `)?$`)

	reDefaultClause = regexp.MustCompile(`(?i)\s+DEFAULT\b`)

	// reDefaultDeny marks a DEFAULT followed by more column options.
	reDefaultDeny = regexp.MustCompile(`(?i)\b(?:CONSTRAINT|GENERATED|STORED|IDENTITY|` +
		`DEFAULT|COMPRESSION|STORAGE)\b`)

	// reQualifiedCall is a schema-qualified function call. Only
	// pg_catalog's own now() and friends are proven stable: app.now()
	// can be any function.
	reQualifiedCall = regexp.MustCompile(`([\w$]+)\s*\.\s*[\w$]+\s*\(`)
)

// addColumnAdditive reports whether one ALTER TABLE subcommand is ADD
// [COLUMN] name type [DEFAULT expr] with a built-in type and a default
// that is a constant or a stable function (now(), current_timestamp):
// metadata-only on PG 11+, the oldest major pg_sage supports being 14.
func addColumnAdditive(sub string) bool {
	m := reAddColumnDef.FindStringSubmatch(sub)
	if m == nil || !isColumnIdent(m[1]) {
		return false
	}
	typ, def := m[2], ""
	hasDefault := false
	if loc := reDefaultClause.FindStringIndex(typ); loc != nil {
		typ, def, hasDefault = typ[:loc[0]], strings.TrimSpace(typ[loc[1]:]), true
	}
	if !reBuiltinType.MatchString(strings.TrimSpace(typ)) {
		return false
	}
	if !hasDefault {
		return true
	}
	return def != "" && !reDefaultDeny.MatchString(def) && !isVolatileDefault(def) &&
		stableCallSpelling(def)
}

// stableCallSpelling reports whether every function call in a default is
// spelled so isVolatileDefault can see it: no quoted identifier (a
// "gen_random_uuid"() call hides from it) and no schema but pg_catalog.
func stableCallSpelling(def string) bool {
	if strings.Contains(def, `"`) {
		return false
	}
	for _, m := range reQualifiedCall.FindAllStringSubmatch(def, -1) {
		if !strings.EqualFold(m[1], "pg_catalog") {
			return false
		}
	}
	return true
}
