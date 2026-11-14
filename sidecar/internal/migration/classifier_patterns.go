package migration

import (
	"regexp"
	"strings"
)

// identRE matches one SQL identifier: "quoted ""x""" or bare_word.
const identRE = `(?:"(?:[^"]|"")+"|[\w$]+)`

// qualifiedRE captures (schema)?(name) with two groups.
const qualifiedRE = `(?:(` + identRE + `)\.)?(` + identRE + `)`

// Compiled patterns — each used by the corresponding match* method.
var (
	// CREATE INDEX (matches any CREATE INDEX; CONCURRENTLY checked separately)
	reCreateIndex = regexp.MustCompile(
		`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\b`)
	reCreateIndexConcurrently = regexp.MustCompile(
		`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)
	reCreateIndexPrefix = regexp.MustCompile(
		`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+`)
	reIndexOnTable = regexp.MustCompile(
		`(?i)\bON\s+(?:ONLY\s+)?` + qualifiedRE)

	// ADD [CONSTRAINT name] CHECK / FOREIGN KEY (without NOT VALID)
	reAddCheckConstraint = regexp.MustCompile(
		`(?i)\bADD\s+(?:CONSTRAINT\s+` + identRE + `\s+)?CHECK\b`)
	reNotValid = regexp.MustCompile(`(?i)\bNOT\s+VALID\b`)
	reAddFK    = regexp.MustCompile(
		`(?i)\bADD\s+(?:CONSTRAINT\s+` + identRE + `\s+)?FOREIGN\s+KEY\b`)

	// ADD [CONSTRAINT name] PRIMARY KEY / UNIQUE builds an index under
	// ACCESS EXCLUSIVE unless it attaches an existing one (USING INDEX).
	reAddKey = regexp.MustCompile(
		`(?i)\bADD\s+(?:CONSTRAINT\s+` + identRE +
			`\s+)?(?:PRIMARY\s+KEY|UNIQUE)\b`)
	reKeyUsingIndex = regexp.MustCompile(
		`(?i)\b(?:PRIMARY\s+KEY|UNIQUE)\s+USING\s+INDEX\b`)

	// ALTER [COLUMN] x SET NOT NULL
	reSetNotNull = regexp.MustCompile(
		`(?i)\bALTER\s+(?:COLUMN\s+)?(` + identRE + `)\s+SET\s+NOT\s+NULL\b`)

	// ALTER [COLUMN] x [SET DATA] TYPE target
	reAlterType = regexp.MustCompile(
		`(?i)\bALTER\s+(?:COLUMN\s+)?(` + identRE +
			`)\s+(?:SET\s+DATA\s+)?TYPE\s+([^,;]+?)` +
			`(?:\s+(?:USING|COLLATE)\b.*)?(?:,|;|$)`)

	// reUsingOrCollate finds the USING or COLLATE clause of an ALTER TYPE
	// match, either of which keeps the change a rewrite.
	reUsingOrCollate = regexp.MustCompile(`(?i)\b(?:USING|COLLATE)\b`)

	// ADD [COLUMN] x type ... DEFAULT expr: the first DEFAULT after the
	// ADD, so matching it at every ADD (reAddHead) reads every clause.
	reAddColumnDefault = regexp.MustCompile(
		`(?i)\bADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(` + identRE +
			`)\s+\S+.*?\bDEFAULT\s+(.+?)(?:\s*,|\s*;|\s*\)|$)`)

	// reAddHead finds each ADD of a multi-clause ALTER TABLE.
	reAddHead = regexp.MustCompile(`(?i)\bADD\s`)

	// ADD [COLUMN] x serial / IDENTITY / GENERATED ... STORED: all
	// populate every existing row and therefore rewrite the table.
	reAddColumnGenerated = regexp.MustCompile(
		`(?i)\bADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(` + identRE +
			`)\s+(?:(?:small|big)?serial[248]?\b|\S+.*\bGENERATED\s+` +
			`(?:ALWAYS|BY\s+DEFAULT)\s+AS\s+(?:IDENTITY\b|\(.*\)\s*STORED\b))`)

	// DROP [COLUMN] [IF EXISTS] x
	reDropColumn = regexp.MustCompile(
		`(?i)\bDROP\s+(COLUMN\s+)?(?:IF\s+EXISTS\s+)?(` + identRE + `)`)

	// DROP TABLE
	reDropTable = regexp.MustCompile(
		`(?i)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?` + qualifiedRE)

	// REINDEX [(opts)] {TABLE|INDEX|...} [CONCURRENTLY] name
	reReindex = regexp.MustCompile(
		`(?i)^\s*REINDEX\b`)
	reReindexConcurrently = regexp.MustCompile(
		`(?i)^\s*REINDEX\s+.*\bCONCURRENTLY\b`)
	reReindexTarget = regexp.MustCompile(
		`(?i)^\s*REINDEX\s+(?:\([^)]*\)\s*)?TABLE\s+(?:CONCURRENTLY\s+)?` +
			qualifiedRE)

	// VACUUM FULL
	reVacuumFull = regexp.MustCompile(
		`(?i)^\s*VACUUM\s+.*\bFULL\b`)
	reVacuumTarget = regexp.MustCompile(
		`(?i)^\s*VACUUM\s+(?:\([^)]*\)\s*)?` +
			`(?:(?:FULL|FREEZE|VERBOSE|ANALYZE|ANALYSE)\s+)*` + qualifiedRE)

	// REFRESH MATERIALIZED VIEW
	reRefreshMatView = regexp.MustCompile(
		`(?i)^\s*REFRESH\s+MATERIALIZED\s+VIEW\b`)
	reRefreshConcurrently = regexp.MustCompile(
		`(?i)^\s*REFRESH\s+MATERIALIZED\s+VIEW\s+CONCURRENTLY\b`)
	reRefreshTarget = regexp.MustCompile(
		`(?i)^\s*REFRESH\s+MATERIALIZED\s+VIEW\s+(?:CONCURRENTLY\s+)?` +
			qualifiedRE)

	// CLUSTER [VERBOSE] table [USING index]
	reCluster = regexp.MustCompile(
		`(?i)^\s*CLUSTER\b`)
	reClusterTarget = regexp.MustCompile(
		`(?i)^\s*CLUSTER\s+(?:\([^)]*\)\s*)?(?:VERBOSE\s+)?` + qualifiedRE)

	// SET TABLESPACE
	reSetTablespace = regexp.MustCompile(
		`(?i)\bSET\s+TABLESPACE\b`)

	// ATTACH PARTITION
	reAttachPartition = regexp.MustCompile(
		`(?i)\bATTACH\s+PARTITION\b`)

	// ALTER TABLE schema.table
	reAlterTable = regexp.MustCompile(
		`(?i)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?` + qualifiedRE)
)

// reservedAfterKeyword lists words that follow ADD/DROP/ALTER inside
// ALTER TABLE but are not column names (COLUMN is optional in SQL).
var reservedAfterKeyword = map[string]bool{
	"constraint": true, "check": true, "foreign": true, "primary": true,
	"unique": true, "exclude": true, "default": true, "not": true,
	"table": true, "index": true, "expression": true, "identity": true,
	"column": true, "statistics": true, "storage": true, "compression": true,
	"full": true, "freeze": true, "verbose": true, "analyze": true,
	"analyse": true, "concurrently": true, "only": true, "if": true,
}

// unquoteIdent applies PostgreSQL identifier rules: quoted identifiers
// keep their case (with "" unescaped); bare ones fold to lower case.
func unquoteIdent(ident string) string {
	if len(ident) >= 2 && ident[0] == '"' && ident[len(ident)-1] == '"' {
		return strings.ReplaceAll(ident[1:len(ident)-1], `""`, `"`)
	}
	return strings.ToLower(ident)
}

// isColumnIdent reports whether a captured identifier is a real column
// name rather than a keyword swallowed by an optional-COLUMN pattern.
func isColumnIdent(ident string) bool {
	if strings.HasPrefix(ident, `"`) {
		return true
	}
	return !reservedAfterKeyword[strings.ToLower(ident)]
}

// immutableDefaults lists functions whose pg_proc.provolatile is 's' or
// 'i' and that are commonly used as column defaults. PostgreSQL only
// skips the table rewrite for non-volatile defaults, so this list must
// never contain a VOLATILE function (G7-B04: gen_random_uuid,
// uuid_generate_v4 and clock_timestamp are VOLATILE). Unknown functions
// are treated as volatile.
var immutableDefaults = map[string]bool{
	"now":                   true,
	"current_timestamp":     true,
	"current_date":          true,
	"current_time":          true,
	"localtime":             true,
	"localtimestamp":        true,
	"transaction_timestamp": true,
	"statement_timestamp":   true,
}

// callIdentRE is one identifier of a function call: bare, or "quoted"
// with "" for a quote (an empty quoted name is kept, as unknown).
const callIdentRE = `"(?:[^"]|"")*"|[\w$]+`

// reFuncCall is a function call: group 1 is the schema (empty when the
// call is unqualified) and group 2 the name.
var reFuncCall = regexp.MustCompile(
	`(?:(` + callIdentRE + `)\s*\.\s*)?(` + callIdentRE + `)\s*\(`)

// isVolatileDefault returns true if the default expression contains a
// function call that is NOT in the stable/immutable allowlist. It reads
// the name as PostgreSQL does: a quoted name keeps its case, and a schema
// other than pg_catalog can name any function, so it counts as volatile
// (the parse-tree mapping's volatileExpr; env-cgo-b).
func isVolatileDefault(expr string) bool {
	for _, m := range reFuncCall.FindAllStringSubmatch(expr, -1) {
		if m[1] != "" && unquoteIdent(m[1]) != "pg_catalog" {
			return true
		}
		if !immutableDefaults[unquoteIdent(m[2])] {
			return true
		}
	}
	return false
}
