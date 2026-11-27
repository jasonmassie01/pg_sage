//go:build cgo

package migration

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// parserAvailable: this build links libpg_query, so DDL is classified
// from its parse tree (astStatement).
const parserAvailable = true

// parseStatement parses one sanitized statement with PostgreSQL's own
// grammar (libpg_query, PG 17).
func parseStatement(stmt string) (*pg_query.ParseResult, error) {
	return pg_query.Parse(stmt)
}
