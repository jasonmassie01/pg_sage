package optimizer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// expressionIndexExtensions are the extensions whose functions an
// expression index may call. A function counts only when it is a member
// of one of them (pg_depend, deptype 'e') in that extension's schema, so
// a function another role plants beside it (extensions usually live in
// public) is never trusted.
var expressionIndexExtensions = []string{
	"btree_gin", "btree_gist", "citext", "cube", "earthdistance",
	"fuzzystrmatch", "hstore", "intarray", "ltree", "pg_trgm", "postgis",
}

// sqlCallKeywords are SQL keywords that may be followed by "(" but are
// grammar, not function calls: they are reserved or column-name keywords,
// which PostgreSQL never binds to a user function unqualified, so they
// are not looked up. TRIM is grammar that PostgreSQL always binds to
// pg_catalog.btrim, ltrim or rtrim (all IMMUTABLE). ILIKE, IS, LIKE and
// SIMILAR are absent on purpose: "similar(x)" calls a function by that
// name, which may be one another role planted.
var sqlCallKeywords = map[string]bool{
	"all": true, "and": true, "any": true, "array": true, "between": true,
	"case": true, "cast": true, "coalesce": true, "distinct": true, "else": true,
	"exists": true, "for": true, "from": true, "greatest": true, "in": true,
	"least": true, "not": true, "nullif": true, "or": true, "row": true,
	"some": true, "symmetric": true, "then": true, "trim": true, "when": true,
}

// identStart and identCont are the PostgreSQL lexer's identifier
// characters: ASCII letters, _, digits and $, and every non-ASCII
// character. pgSpace adds \v, which PostgreSQL 16+ reads as whitespace.
const (
	identStart = `[A-Za-z_\x{80}-\x{10FFFF}]`
	identCont  = `[A-Za-z0-9_$\x{80}-\x{10FFFF}]`
	pgSpace    = `[\s\v]`
)

// funcCallRe finds each call: an optional unquoted schema qualifier and
// the unquoted name. Matching is leftmost, so a qualifier is never split
// off its name.
var funcCallRe = regexp.MustCompile(`(?:(` + identStart + identCont + `*)` +
	pgSpace + `*\.` + pgSpace + `*)?(` + identStart + identCont + `*)` + pgSpace + `*\(`)

// unverifiableCallRe finds what funcCallRe cannot see: a quoted name used
// as a function or a schema qualifier (also U&"..." UESCAPE '!' before
// "("), and comments (PostgreSQL accepts "f/**/(x)"). Quoted names are
// refused rather than parsed, because a string literal holding '"' could
// otherwise read as a quoted name that swallows a real call.
var unverifiableCallRe = regexp.MustCompile(
	`(?i)"` + pgSpace + `*[.(]|/\*|--|\buescape\b`)

// foldsUnlikePostgres reports a character that normalizeFragment changes
// but the PostgreSQL lexer keeps inside an identifier: non-ASCII
// whitespace, which is collapsed (so "lower" + U+00A0 + "(" reads as
// lower), and the two characters Go lower-cases to ASCII, U+0130 and the
// Kelvin sign (so "TR" + U+0130 + "M(" reads as trim).
func foldsUnlikePostgres(ddl string) bool {
	for _, r := range ddl {
		if r >= utf8.RuneSelf && (unicode.IsSpace(r) || unicode.ToLower(r) < utf8.RuneSelf) {
			return true
		}
	}
	return false
}

// funcCall is one function an index expression calls; schema is "" when
// the call is unqualified.
type funcCall struct{ schema, name string }

func (c funcCall) String() string {
	if c.schema == "" {
		return c.name
	}
	return c.schema + "." + c.name
}

// functionCalls lists the distinct calls in an index fragment, skipping
// unqualified SQL keywords.
func functionCalls(frag string) []funcCall {
	seen := map[funcCall]bool{}
	var calls []funcCall
	for _, m := range funcCallRe.FindAllStringSubmatch(frag, -1) {
		c := funcCall{schema: m[1], name: m[2]}
		if seen[c] || (c.schema == "" && sqlCallKeywords[c.name]) {
			continue
		}
		seen[c] = true
		calls = append(calls, c)
	}
	return calls
}

// functionSchemasSQL reads, per schema, the functions of a name an index
// expression may call (restricted to the call's schema when it is
// qualified, $3): whether every one is IMMUTABLE, and whether every one is
// trusted, that is in pg_catalog or a member (pg_depend, deptype 'e') of an
// allowlisted extension in that extension's own schema. A function another
// role plants (public.lower, or an overload beside an extension's own)
// leaves its schema untrusted.
const functionSchemasSQL = `SELECT COALESCE(array_agg(x.nspname ORDER BY x.nspname), '{}'),
  COALESCE(array_agg(x.immutable ORDER BY x.nspname), '{}'),
  COALESCE(array_agg(x.trusted ORDER BY x.nspname), '{}')
FROM (SELECT n.nspname::text AS nspname, bool_and(p.provolatile = 'i') AS immutable,
    bool_and(n.nspname = 'pg_catalog' OR EXISTS (
      SELECT 1 FROM pg_catalog.pg_depend d
      JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
      WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass
        AND d.objid = p.oid
        AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
        AND d.deptype = 'e'
        AND e.extnamespace = p.pronamespace
        AND e.extname = ANY($2::text[]))) AS trusted
  FROM pg_catalog.pg_proc p
  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE p.proname = $1
    AND ($3::text = '' OR n.nspname = $3::text)
  GROUP BY n.nspname) x`

// schemaFunctions describes the functions of one name in one schema.
type schemaFunctions struct {
	schema             string
	immutable, trusted bool
}

// functionSchemas reads where fn's name is defined, in a bounded
// read-only catalog transaction.
func (v *Validator) functionSchemas(ctx context.Context, fn funcCall) ([]schemaFunctions, error) {
	reader := catalogread.New(v.catalog, v.timeouts)
	extensions := v.extensions
	if extensions == nil {
		extensions = []string{}
	}
	var schemas []string
	var immutable, trusted []bool
	err := reader.QueryRow(ctx, functionSchemasSQL, fn.name, extensions, fn.schema).
		Scan(&schemas, &immutable, &trusted)
	if err != nil {
		return nil, err
	}
	if len(immutable) != len(schemas) || len(trusted) != len(schemas) {
		return nil, fmt.Errorf("catalog returned %d schemas, %d volatility and %d trust flags",
			len(schemas), len(immutable), len(trusted))
	}
	out := make([]schemaFunctions, len(schemas))
	for i := range schemas {
		out[i] = schemaFunctions{schema: schemas[i], immutable: immutable[i], trusted: trusted[i]}
	}
	return out, nil
}

// bindFunction chooses the schema a call binds to and returns it, or a
// refusal reason. A qualified call binds its own schema, which must be
// trusted. A bare call binds pg_catalog when pg_catalog has the name (the
// only schema the executor's pinned search_path searches for functions),
// otherwise the one trusted extension schema that defines it. Every
// function of the name in the bound schema must be IMMUTABLE.
func bindFunction(fn funcCall, found []schemaFunctions) (string, string) {
	var bound *schemaFunctions
	var trusted []string
	for i := range found {
		f := &found[i]
		if f.schema == fn.schema || (fn.schema == "" && f.schema == "pg_catalog") {
			bound = f
			break
		}
		if fn.schema == "" && f.trusted {
			trusted = append(trusted, f.schema)
			bound = f
		}
	}
	switch {
	case fn.schema == "" && bound != nil && bound.schema != "pg_catalog" && len(trusted) > 1:
		return "", "function_schema_not_allowed: " + fn.String() + " is defined by several " +
			"allowlisted extensions (" + strings.Join(trusted, ", ") + "); name its schema"
	case bound == nil:
		return "", "function_schema_not_allowed: " + fn.String() +
			" has no function in pg_catalog or an allowlisted extension"
	case !bound.trusted:
		return "", "function_schema_not_allowed: " + fn.String() + " resolves to " +
			bound.schema + ", which holds a function of that name outside pg_catalog " +
			"and the allowlisted extensions"
	case !bound.immutable:
		return "", "function " + fn.String() + " is not IMMUTABLE in every pg_catalog or " +
			"allowlisted extension overload"
	}
	return bound.schema, ""
}
