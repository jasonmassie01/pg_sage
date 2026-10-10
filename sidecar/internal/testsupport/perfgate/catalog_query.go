package perfgate

import (
	"regexp"
	"strings"
)

// sqlNoise matches the parts of a statement that are not SQL: comments
// (the pg_sage tag among them) and string literals.
var sqlNoise = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*|'(?:[^']|'')*'`)

// sageObject matches a reference to an object in the sage schema (not
// pg_sage, which is a word of its own).
var sageObject = regexp.MustCompile(`(?i)(?:\bsage|"sage")\s*\.`)

// catalogName matches a name of the system catalog or statistics views:
// any object qualified by pg_catalog or information_schema, or one of the
// catalog relations pg_sage reads unqualified (pg_class, pg_stat_*).
var catalogName = regexp.MustCompile(`(?i)\b(?:pg_catalog|information_schema)\.` +
	`[a-z_][a-z0-9_$]*|` +
	`\bpg_(class|index|indexes|namespace|attribute|attrdef|constraint|sequences?|locks|` +
	`settings|database|roles|authid|tables|tablespace|inherits|partitioned_table|proc|` +
	`type|extension|replication_slots|prepared_xacts|depend|trigger|description|am|` +
	`opclass|views|matviews|shdepend|publication|subscription|stat[a-z_]*)\b`)

// rowSourceKeyword matches text that ends where a row source starts.
var rowSourceKeyword = regexp.MustCompile(`(?i)\b(?:from|join|lateral)\s*$`)

// IsCatalogQuery reports whether a statement reads the system catalog or
// the statistics views and no sage table. A statement that reads a sage
// table is pg_sage history, never a catalog read, whatever else it joins.
// A catalog name followed by "(" is a function call (pg_catalog.now(),
// pg_stat_statements_reset(), pg_catalog.pg_database_size()), not a
// relation, unless it is a system function read as a row source
// (systemRowSource). Comments and string literals are not read.
func IsCatalogQuery(q string) bool {
	sql := sqlNoise.ReplaceAllString(q, " ")
	if sageObject.MatchString(sql) {
		return false
	}
	for _, m := range catalogName.FindAllStringIndex(sql, -1) {
		call := strings.HasPrefix(strings.TrimLeft(sql[m[1]:], " \t\r\n"), "(")
		if !call || systemRowSource(sql[:m[0]], sql[m[0]:m[1]]) {
			return true
		}
	}
	return false
}

// systemRowSource reports whether a call of the function name, preceded
// by before, reads a system function (pg_*) as a row source: FROM
// pg_catalog.pg_ls_waldir() lists the WAL directory and FROM
// pg_stat_get_activity(NULL) every backend, so like a catalog relation
// its cost follows the size of the system it lists. pg_catalog's other
// row sources (unnest, generate_series) read no system state.
func systemRowSource(before, name string) bool {
	fn := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	return strings.HasPrefix(fn, "pg_") && rowSourceKeyword.MatchString(before)
}
