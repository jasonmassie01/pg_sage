package optimizer

import (
	"regexp"

	"github.com/pg-sage/sidecar/internal/sanitize"
)

// qualifyBoundCalls names, on every bare call in ddl's key list and
// predicate, the schema bound[call] says it binds to. Grammar forms are
// left bare: the grammar binds them to pg_catalog itself. A bare call the
// binding does not cover (the name match and the call scanner disagree)
// refuses the index rather than leaving a call unverified.
func qualifyBoundCalls(ddl string, bound map[funcCall]string) (string, string) {
	if len(bound) == 0 {
		return ddl, ""
	}
	refs, err := IndexFunctionCalls(ddl)
	if err != nil {
		return "", "function_schema_not_allowed: the index expression cannot be verified: " +
			err.Error()
	}
	out := ddl
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		if r.Schema != "" || r.Grammar() {
			continue
		}
		schema, ok := bound[funcCall{name: r.Name}]
		if !ok {
			return "", "function_schema_not_allowed: the call to " + r.Name +
				" in the index expression could not be verified"
		}
		out = out[:r.Start] + qualifierFor(schema) + "." + out[r.Start:]
	}
	return out, ""
}

var plainSchemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// reservedWords are the PostgreSQL reserved keywords, which need quotes
// as a schema qualifier.
var reservedWords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "both": true,
	"case": true, "cast": true, "check": true, "collate": true, "column": true,
	"constraint": true, "create": true, "current_catalog": true, "current_date": true,
	"current_role": true, "current_time": true, "current_timestamp": true,
	"current_user": true, "default": true, "deferrable": true, "desc": true,
	"distinct": true, "do": true, "else": true, "end": true, "except": true,
	"false": true, "fetch": true, "for": true, "foreign": true, "from": true,
	"grant": true, "group": true, "having": true, "in": true, "initially": true,
	"intersect": true, "into": true, "lateral": true, "leading": true, "limit": true,
	"localtime": true, "localtimestamp": true, "not": true, "null": true,
	"offset": true, "on": true, "only": true, "or": true, "order": true,
	"placing": true, "primary": true, "references": true, "returning": true,
	"select": true, "session_user": true, "some": true, "symmetric": true,
	"system_user": true, "table": true, "then": true, "to": true, "trailing": true,
	"true": true, "union": true, "unique": true, "user": true, "using": true,
	"variadic": true, "when": true, "where": true, "window": true, "with": true,
}

// qualifierFor writes a schema as a call qualifier: bare when PostgreSQL
// reads it unquoted as written (pg_catalog, public), quoted otherwise.
func qualifierFor(schema string) string {
	if plainSchemaName.MatchString(schema) && !reservedWords[schema] {
		return schema
	}
	return sanitize.QuoteIdentifier(schema)
}

// catalogQualifier is an unquoted pg_catalog qualifier that does not end
// a longer name ("my_pg_catalog.f" keeps its qualifier).
var catalogQualifier = regexp.MustCompile(
	`(^|[^A-Za-z0-9_$."\x{80}-\x{10FFFF}])pg_catalog[ \t\n\r\f\v]*\.[ \t\n\r\f\v]*`)

// dropCatalogQualifiers removes pg_catalog qualifiers from a normalized
// fragment, so pg_catalog.lower(email) compares and fingerprints like the
// lower(email) pg_get_indexdef prints and earlier findings recorded.
func dropCatalogQualifiers(frag string) string {
	return catalogQualifier.ReplaceAllString(frag, "$1")
}
