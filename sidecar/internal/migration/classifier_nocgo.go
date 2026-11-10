//go:build !cgo

package migration

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// parserAvailable: without cgo there is no libpg_query, so DDL is
// classified by the regex rules and the additive allowlist, and the first
// classification logs that it is degraded (noteDegraded).
const parserAvailable = false

// parseStatement has no parser to call; astStatement then treats every
// statement as unparsed, which is unrecognized (fail closed).
func parseStatement(string) (*pg_query.ParseResult, error) {
	return nil, sqlast.ErrUnavailable
}
