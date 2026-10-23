//go:build integration && cgo

package sqlast

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func connectTestDB(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if conn.Config().DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		t.Fatalf("query exec mode %v is not the extended protocol default",
			conn.Config().DefaultQueryExecMode)
	}
	return conn, ctx
}

func execAll(t *testing.T, ctx context.Context, conn *pgx.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// statementsText returns the pg_stat_statements text this database keeps
// for the one statement whose text contains marker.
func statementsText(t *testing.T, ctx context.Context, conn *pgx.Conn, marker string) string {
	t.Helper()
	rows, err := conn.Query(ctx, `/* pg_sage sqlast_test v1 */
SELECT query FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND strpos(query, $1) > 0 AND query LIKE 'SELECT %'`, marker)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect pg_stat_statements rows: %v", err)
	}
	if len(texts) != 1 {
		t.Fatalf("pg_stat_statements has %d entries for %q, want 1: %q",
			len(texts), marker, texts)
	}
	return texts[0]
}

// CHECK-SQL-04 (server half): the fingerprint of the text PostgreSQL 14-18
// records in pg_stat_statements for a statement sent over the extended
// protocol equals the fingerprint of what the application sent.
func TestFingerprintMatchesPgStatStatementsText(t *testing.T) {
	conn, ctx := connectTestDB(t)
	// A fresh schema per run: a recreated table gets a new OID and so a new
	// queryid, and old entries must not match.
	schema := fmt.Sprintf("sqlast_fp_%d", time.Now().UnixNano())
	execAll(t, ctx, conn,
		"CREATE EXTENSION IF NOT EXISTS pg_stat_statements",
		"CREATE SCHEMA "+schema,
		"CREATE TABLE "+schema+".t (id int, code int)",
		"CREATE TABLE "+schema+".p (id int, code int)")
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	literal := "SELECT * FROM " + schema + ".t WHERE id = 42 /* app='x' */"
	execAll(t, ctx, conn, literal)
	param := "SELECT * FROM " + schema + ".p WHERE id = $1"
	if _, err := conn.Exec(ctx, param, 42); err != nil {
		t.Fatalf("parameterized query: %v", err)
	}
	recordedLiteral := statementsText(t, ctx, conn, schema+".t")
	recordedParam := statementsText(t, ctx, conn, schema+".p")
	if recordedLiteral == literal {
		t.Fatalf("pg_stat_statements kept the literal %q; normalization not active", literal)
	}
	if got, want := mustFingerprint(t, recordedLiteral), mustFingerprint(t, literal); got != want {
		t.Errorf("Fingerprint(%q) = %s, want %s (sent %q)", recordedLiteral, got, want, literal)
	}
	if got, want := mustFingerprint(t, recordedParam),
		mustFingerprint(t, "SELECT * FROM "+schema+".p WHERE id = 42"); got != want {
		t.Errorf("Fingerprint(%q) = %s, want %s", recordedParam, got, want)
	}
	if mustFingerprint(t, recordedLiteral) ==
		mustFingerprint(t, "SELECT * FROM "+schema+".t WHERE code = $1") {
		t.Error("a different predicate column matched the recorded statement")
	}
}

// CHECK-SQL-07: the PG18-only statements InspectDDL rejects really are
// PG18-only: PostgreSQL 18 accepts them, earlier servers reject them too.
func TestPG18OnlySyntaxMatchesServer(t *testing.T) {
	conn, ctx := connectTestDB(t)
	var version int
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&version); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	stmts := []string{
		"CREATE TEMP TABLE sqlast_v18 (a int, b int GENERATED ALWAYS AS (a * 2) VIRTUAL)",
		"CREATE TEMP TABLE sqlast_ne (a int, CONSTRAINT c CHECK (a > 0) NOT ENFORCED)",
	}
	for _, sql := range stmts {
		if _, err := InspectDDL(sql); !errors.Is(err, ErrRejected) {
			t.Errorf("InspectDDL(%q) error = %v, want ErrRejected", sql, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, execErr := tx.Exec(ctx, sql)
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		switch {
		case version >= 180000 && execErr != nil:
			t.Errorf("PG %d rejected %q: %v", version, sql, execErr)
		case version < 180000 && (!errors.As(execErr, &pgErr) || pgErr.Code != "42601"):
			t.Errorf("PG %d: %q error = %v, want syntax error 42601", version, sql, execErr)
		}
	}
}
