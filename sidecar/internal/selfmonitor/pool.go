package selfmonitor

import (
	"context"
	"fmt"
	"net"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigurePool makes a pool's sessions recognizable as pg_sage's own:
// application_name pg_sage (pg_stat_activity) and every statement tagged
// with StatementTag (pg_stat_statements). pg_sage does not hide from
// pg_stat_statements: its cost is visible to the DBA, and its own readers
// exclude it with StatementExclusionSQL and ActivityExclusionSQL. Every
// session is pinned to READ COMMITTED (pinReadCommitted). An
// AfterNetConnect or AfterConnect hook already set runs first.
func ConfigurePool(cfg *pgxpool.Config) {
	if cfg == nil {
		return
	}
	ConfigureConn(cfg.ConnConfig)
}

// ConfigureConn does for a single connection's configuration what
// ConfigurePool does for a pool's, for the connections pg_sage opens
// outside a pool.
func ConfigureConn(cfg *pgx.ConnConfig) {
	if cfg == nil {
		return
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = ApplicationName
	inner := cfg.AfterNetConnect
	cfg.AfterNetConnect = func(
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
	pinReadCommitted(cfg)
	if pinPathHarness {
		pinSearchPath(cfg)
	}
}

// pinReadCommittedSQL makes every transaction of the session READ
// COMMITTED, whatever the role or database default is: the audit chain's
// head probe needs a new snapshot per statement (audit-02). It is a SET,
// not a startup parameter, because PgBouncer refuses startup parameters
// it does not know; PgBouncer pools per user and database, so it reaches
// only pg_sage's own server connections.
const pinReadCommittedSQL = "SET SESSION default_transaction_isolation = 'read committed'"

// pinReadCommitted chains the pin after any AfterConnect hook already set,
// so an earlier hook cannot undo it. A pin that fails fails the connection:
// pg_sage never runs on a session it could not pin.
func pinReadCommitted(cfg *pgx.ConnConfig) {
	inner := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		if inner != nil {
			if err := inner(ctx, pc); err != nil {
				return err
			}
		}
		if err := pc.Exec(ctx, pinReadCommittedSQL).Close(); err != nil {
			return fmt.Errorf("pin default_transaction_isolation to read committed: %w", err)
		}
		return nil
	}
}

// PinSearchPathSQL is the observation sessions' search_path pin: only
// pg_catalog and the session's own temporary schema, so no object a
// database user creates resolves unless it is named with its schema
// (security-01c). It is a SET, not a startup parameter, for PgBouncer, and
// so pg_settings.reset_val (which apppath reads) keeps the server's value.
const PinSearchPathSQL = "SET SESSION search_path = pg_catalog, pg_temp"

// pinSearchPath chains the search_path pin after every other AfterConnect
// hook (the READ COMMITTED pin first). A pin that fails fails the
// connection, naming search_path.
func pinSearchPath(cfg *pgx.ConnConfig) {
	inner := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		if inner != nil {
			if err := inner(ctx, pc); err != nil {
				return err
			}
		}
		if err := pc.Exec(ctx, PinSearchPathSQL).Close(); err != nil {
			return fmt.Errorf("pin search_path to pg_catalog, pg_temp: %w", err)
		}
		return nil
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

// IsStatementText reports a statement text StatementExclusionSQL leaves
// out (pg_sage's own), for readers that filter texts in Go: ILIKE
// '%pg_sage%' (where _ is any one character) or a sage schema reference.
// SQL NULL is the empty string there, which is not pg_sage's. Letters
// outside ASCII count as separators here; PostgreSQL's [[:alnum:]] follows
// the database's locale, so the two can differ only on such a letter right
// before "sage.".
func IsStatementText(text string) bool {
	return applicationNamePattern.MatchString(text) || sageSchemaRef.MatchString(text)
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
