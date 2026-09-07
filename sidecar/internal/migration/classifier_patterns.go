package migration

import (
	"regexp"
	"strings"
)

// Compiled patterns — each used by the corresponding match* method.
var (
	// CREATE INDEX (matches any CREATE INDEX; CONCURRENTLY checked separately)
	reCreateIndex = regexp.MustCompile(
		`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\b`)
	reCreateIndexConcurrently = regexp.MustCompile(
		`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)
	reIndexOnTable = regexp.MustCompile(
		`(?i)\bON\s+((?:(\w+)\.)?(\w+))`)

	// ADD CONSTRAINT ... CHECK (without NOT VALID)
	reAddCheckConstraint = regexp.MustCompile(
		`(?i)\bADD\s+CONSTRAINT\s+\w+\s+CHECK\b`)
	reNotValid = regexp.MustCompile(`(?i)\bNOT\s+VALID\b`)

	// ADD CONSTRAINT ... FOREIGN KEY (without NOT VALID)
	reAddFK = regexp.MustCompile(
		`(?i)\bADD\s+CONSTRAINT\s+\w+\s+FOREIGN\s+KEY\b`)

	// ALTER COLUMN ... SET NOT NULL
	reSetNotNull = regexp.MustCompile(
		`(?i)\bALTER\s+COLUMN\s+(\w+)\s+SET\s+NOT\s+NULL\b`)

	// ALTER COLUMN ... TYPE
	reAlterType = regexp.MustCompile(
		`(?i)\bALTER\s+COLUMN\s+(\w+)\s+(?:SET\s+DATA\s+)?TYPE\b`)

	// ADD COLUMN ... DEFAULT (volatile)
	reAddColumnDefault = regexp.MustCompile(
		`(?i)\bADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+\S+.*\bDEFAULT\s+(.+?)(?:\s*,|\s*;|\s*\)|$)`)

	// ADD COLUMN ... NOT NULL without DEFAULT
	reAddColumnNotNull = regexp.MustCompile(
		`(?i)\bADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+\S+\s+NOT\s+NULL\b`)
	reHasDefault = regexp.MustCompile(`(?i)\bDEFAULT\b`)

	// DROP COLUMN
	reDropColumn = regexp.MustCompile(
		`(?i)\bDROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(\w+)`)

	// DROP TABLE
	reDropTable = regexp.MustCompile(
		`(?i)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?((?:(\w+)\.)?(\w+))`)

	// REINDEX
	reReindex = regexp.MustCompile(
		`(?i)^\s*REINDEX\b`)
	reReindexConcurrently = regexp.MustCompile(
		`(?i)^\s*REINDEX\s+.*\bCONCURRENTLY\b`)

	// VACUUM FULL
	reVacuumFull = regexp.MustCompile(
		`(?i)^\s*VACUUM\s+.*\bFULL\b`)

	// REFRESH MATERIALIZED VIEW
	reRefreshMatView = regexp.MustCompile(
		`(?i)^\s*REFRESH\s+MATERIALIZED\s+VIEW\b`)
	reRefreshConcurrently = regexp.MustCompile(
		`(?i)^\s*REFRESH\s+MATERIALIZED\s+VIEW\s+CONCURRENTLY\b`)

	// CLUSTER
	reCluster = regexp.MustCompile(
		`(?i)^\s*CLUSTER\b`)

	// SET TABLESPACE
	reSetTablespace = regexp.MustCompile(
		`(?i)\bSET\s+TABLESPACE\b`)

	// ATTACH PARTITION
	reAttachPartition = regexp.MustCompile(
		`(?i)\bATTACH\s+PARTITION\b`)

	// ALTER TABLE schema.table
	reAlterTable = regexp.MustCompile(
		`(?i)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?((?:(\w+)\.)?(\w+))`)

	// SET lock_timeout
	reLockTimeout = regexp.MustCompile(
		`(?i)\bSET\s+(?:LOCAL\s+)?lock_timeout\b`)
)

// Volatile default detection — allowlisted immutable/stable funcs.
var immutableDefaults = map[string]bool{
	"now":                true,
	"current_timestamp":  true,
	"clock_timestamp":    true,
	"gen_random_uuid":    true,
	"uuid_generate_v4":  true,
	"current_date":      true,
	"current_time":      true,
	"localtime":         true,
	"localtimestamp":     true,
	"transaction_timestamp": true,
	"statement_timestamp":   true,
}

var reFuncCall = regexp.MustCompile(`(?i)(\w+)\s*\(`)

// isVolatileDefault returns true if the default expression contains a
// function call that is NOT in the stable/immutable allowlist.
// Note: for purposes of table rewrite detection, we treat the
// allowlisted functions as "safe" on PG11+ (they don't cause rewrite).
// On PG < 11, ANY default causes a rewrite.
func isVolatileDefault(expr string) bool {
	matches := reFuncCall.FindAllStringSubmatch(expr, -1)
	for _, m := range matches {
		fname := strings.ToLower(m[1])
		if !immutableDefaults[fname] {
			return true
		}
	}
	return false
}
