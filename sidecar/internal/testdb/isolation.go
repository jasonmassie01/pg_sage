package testdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// isolationLevels are the default_transaction_isolation values
// SetIsolationDefault accepts. ALTER ROLE takes no parameters, so the
// value is spliced from this allowlist only.
var isolationLevels = map[string]bool{
	"read uncommitted": true, "read committed": true,
	"repeatable read": true, "serializable": true,
}

// SetIsolationDefault makes level the default_transaction_isolation of the
// connecting role in dsn's database (ALTER ROLE ... IN DATABASE), the way
// an operator's role default reaches pg_sage's sessions. Use it on a
// database from CreateDatabase: dropping the database drops the setting,
// so no other test sees it. Only connections opened afterwards get it.
func SetIsolationDefault(t testing.TB, dsn, level string) {
	t.Helper()
	if !isolationLevels[level] {
		t.Fatalf("SetIsolationDefault: unknown isolation level %q", level)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("SetIsolationDefault: connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var role, db string
	if err := conn.QueryRow(ctx, "SELECT session_user, current_database()").
		Scan(&role, &db); err != nil {
		t.Fatalf("SetIsolationDefault: read role and database: %v", err)
	}
	sql := fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET default_transaction_isolation = '%s'",
		pgx.Identifier{role}.Sanitize(), pgx.Identifier{db}.Sanitize(), level)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("SetIsolationDefault: %s: %v", sql, err)
	}
}
