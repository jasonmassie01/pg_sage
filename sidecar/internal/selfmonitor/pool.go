package selfmonitor

import (
	"context"
	"net"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigurePool makes a pool's sessions recognizable as pg_sage's own:
// application_name pg_sage (pg_stat_activity) and every statement tagged
// with StatementTag (pg_stat_statements). pg_sage does not hide from
// pg_stat_statements: its cost is visible to the DBA, and its own readers
// exclude it with StatementExclusionSQL and ActivityExclusionSQL. An
// AfterNetConnect hook already set (a proxy handshake) runs first.
func ConfigurePool(cfg *pgxpool.Config) {
	if cfg == nil || cfg.ConnConfig == nil {
		return
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = ApplicationName
	inner := cfg.ConnConfig.AfterNetConnect
	cfg.ConnConfig.AfterNetConnect = func(
		ctx context.Context, pc *pgconn.Config, conn net.Conn,
	) (net.Conn, error) {
		if inner != nil {
			var err error
			if conn, err = inner(ctx, pc, conn); err != nil {
				return nil, err
			}
		}
		if conn == nil {
			return nil, errNilConn
		}
		return TagConn(conn), nil
	}
}

// StatementExclusionSQL is a predicate, true for a statement text that is
// not pg_sage's own: no pg_sage tag and no reference to the sage schema.
// column is the text expression (default query).
func StatementExclusionSQL(column string) string {
	if column == "" {
		column = "query"
	}
	text := "COALESCE(" + column + ", '')"
	return text + " NOT ILIKE '%pg_sage%' AND " + text + " !~* '" + QueryTextSQLRegex + "'"
}

// ActivityExclusionSQL is a predicate over pg_stat_activity, true for a
// session that is not pg_sage's own (every pg_sage pool sets
// application_name). alias qualifies the column; empty for none.
func ActivityExclusionSQL(alias string) string {
	col := "application_name"
	if alias != "" {
		col = alias + "." + col
	}
	return "COALESCE(" + col + ", '') NOT ILIKE '%" + ApplicationName + "%'"
}

// applicationNamePattern is ActivityExclusionSQL's ILIKE pattern
// ('%pg_sage%', where _ is any one character) as a regular expression.
var applicationNamePattern = regexp.MustCompile(`(?is)pg.sage`)

// IsApplicationName reports a session name ActivityExclusionSQL leaves
// out (pg_sage's own), for sources that carry application_name outside
// SQL: log records and auto_explain plans.
func IsApplicationName(name string) bool {
	return applicationNamePattern.MatchString(name)
}
