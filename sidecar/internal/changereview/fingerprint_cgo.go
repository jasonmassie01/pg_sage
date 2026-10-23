//go:build cgo

package changereview

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// maxParseBytes matches the SQL parser's input bound: larger scripts are
// keyed by text.
const maxParseBytes = 100000

// astKeys keys each statement of sql by its deparsed parse tree, which
// drops comments, whitespace and keyword case but keeps every literal and
// type modifier (unlike a query fingerprint, which ignores constants: a
// verdict must not carry over a changed lock_timeout or column length).
// ok is false when sql does not parse.
//
// It calls libpg_query directly so the foundation does not wait for the
// sqlast helpers; sqlast.Fingerprint serves statement-to-workload
// matching, a different question.
func astKeys(sql string) ([]string, bool) {
	if len(sql) > maxParseBytes {
		return nil, false
	}
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(tree.GetStmts()))
	for _, raw := range tree.GetStmts() {
		one := &pg_query.ParseResult{Version: tree.GetVersion(),
			Stmts: []*pg_query.RawStmt{{Stmt: raw.GetStmt()}}}
		text, err := pg_query.Deparse(one)
		if err != nil {
			return nil, false
		}
		keys = append(keys, "a:"+sha256Hex([]byte(text)))
	}
	return keys, true
}
