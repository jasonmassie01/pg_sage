//go:build cgo

package sqlast

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Fingerprint identifies a statement's shape independent of literals, $n
// parameter numbers, comments, case and whitespace:
// pg_query.Fingerprint(pg_query.Normalize(sql)), 16 hex digits. Statement
// text from pg_stat_statements and the SQL an application sends fingerprint
// alike. An input without a statement is rejected.
func Fingerprint(sql string) (string, error) {
	tree, err := parseScript(sql)
	if err != nil {
		return "", err
	}
	if len(tree.GetStmts()) == 0 {
		return "", reject("no statement")
	}
	parseCalls.Add(1)
	normalized, err := pg_query.Normalize(sql)
	if err != nil {
		return "", parseError(sql, err)
	}
	parseCalls.Add(1)
	fingerprint, err := pg_query.Fingerprint(normalized)
	if err != nil {
		return "", parseError(normalized, err)
	}
	return fingerprint, nil
}
