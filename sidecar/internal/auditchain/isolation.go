package auditchain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// isolationDefaultSQL reads the default_transaction_isolation a session
// would have without pg_sage's read committed pin. reset_val is the value
// from before any SET (server configuration, the database, the role and
// the connection's options), so the pin does not mask it the way it masks
// SHOW. sources names the role and database settings that set it.
const isolationDefaultSQL = `/* pg_sage audit_chain v1 */
SELECT s.reset_val, session_user::text, current_database()::text,
    COALESCE((SELECT string_agg(CASE
            WHEN d.setrole = 0 AND d.setdatabase = 0 THEN 'every role'
            WHEN d.setrole = 0 THEN 'database ' || current_database()
            WHEN d.setdatabase = 0 THEN 'role ' || session_user
            ELSE 'role ' || session_user || ' in database ' || current_database()
        END || ' = ' || substr(c, length('default_transaction_isolation=') + 1),
        '; ' ORDER BY d.setrole DESC, d.setdatabase DESC)
     FROM pg_db_role_setting d, unnest(d.setconfig) c
     WHERE d.setrole IN (0, (SELECT oid FROM pg_roles WHERE rolname = session_user))
       AND d.setdatabase IN (0,
           (SELECT oid FROM pg_database WHERE datname = current_database()))
       AND c LIKE 'default_transaction_isolation=%'), '')
FROM pg_settings s WHERE s.name = 'default_transaction_isolation'`

// IsolationWarning is the startup self-check behind the chain's read
// committed requirement: a warning when the role or database default is
// another level (pg_sage's sessions are pinned, but a transaction pooler
// can drop the pin), "" when the default is safe.
func IsolationWarning(ctx context.Context, q Querier) (string, error) {
	var level, role, db, sources string
	if err := q.QueryRow(ctx, isolationDefaultSQL).Scan(&level, &role, &db, &sources); err != nil {
		return "", fmt.Errorf("read the default_transaction_isolation of this role "+
			"and database: %w", err)
	}
	return isolationWarning(level, role, db, sources), nil
}

// snapshotPerStatement reports a level that takes a new snapshot for every
// statement, as the chain's head probe needs. PostgreSQL runs READ
// UNCOMMITTED exactly as READ COMMITTED.
func snapshotPerStatement(level string) bool {
	switch strings.ToLower(level) {
	case "read committed", "read uncommitted":
		return true
	}
	return false
}

func isolationWarning(level, role, db, sources string) string {
	if snapshotPerStatement(level) {
		return ""
	}
	if sources == "" {
		sources = "server configuration or connection options"
	}
	return fmt.Sprintf("default_transaction_isolation defaults to '%s' for role %s in "+
		"database %s (set by %s). pg_sage pins its own sessions to read committed, "+
		"but a transaction pooler can drop that pin; audit chain writes on an "+
		"unpinned session then fail with SQLSTATE 55000. Fix: ALTER ROLE %s IN "+
		"DATABASE %s SET default_transaction_isolation = 'read committed'",
		level, pgx.Identifier{role}.Sanitize(), pgx.Identifier{db}.Sanitize(), sources,
		pgx.Identifier{role}.Sanitize(), pgx.Identifier{db}.Sanitize())
}
