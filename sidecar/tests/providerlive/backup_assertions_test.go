//go:build providerlive

package providerlive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func backupBinary(t *testing.T, key, fallback string) string {
	name := os.Getenv(key)
	if name == "" {
		name = fallback
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skip(key + " or client binary required; restore NOT verified")
	}
	return path
}

func runBackupTool(t *testing.T, binary string, pool *pgxpool.Pool, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	conn := pool.Config().ConnConfig
	ssl := "disable"
	if conn.TLSConfig != nil {
		ssl = "require"
	}
	cmd.Env = []string{"PGHOST=" + conn.Host, "PGPORT=" + strconv.Itoa(int(conn.Port)),
		"PGUSER=" + conn.User, "PGPASSWORD=" + conn.Password, "PGDATABASE=" + conn.Database,
		"PGSSLMODE=" + ssl, "PGCONNECT_TIMEOUT=15"}
	for _, key := range []string{"SystemRoot", "PATH", "TEMP", "TMP", "HOME", "APPDATA"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	// Client diagnostics may include credentials or SQL; emit only stable failure classification.
	if err := cmd.Run(); err != nil {
		checkError(t, "execute backup client", err)
	}
}

func verifyRestoredFixture(t *testing.T, f fixture, restored *pgxpool.Pool) {
	query := "SELECT count(*), md5(string_agg(id::text||label||score::text||" +
		"embedding::text, ',' ORDER BY id)) FROM " + f.table("records")
	var sourceCount, restoredCount int
	var sourceHash, restoredHash string
	checkError(t, "read source content hash", f.pool.QueryRow(t.Context(), query).
		Scan(&sourceCount, &sourceHash))
	checkError(t, "read restored content hash", restored.QueryRow(t.Context(), query).
		Scan(&restoredCount, &restoredHash))
	if sourceCount != 100 || restoredCount != sourceCount || sourceHash != restoredHash {
		t.Fatal("restored row/vector values differ from source")
	}
	var indexes, constraints, children int
	checkError(t, "read restored indexes", restored.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_indexes WHERE schemaname=$1", f.namespace).Scan(&indexes))
	checkError(t, "read restored constraints", restored.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace "+
			"WHERE n.nspname=$1", f.namespace).Scan(&constraints))
	checkError(t, "read restored children", restored.QueryRow(t.Context(),
		"SELECT count(*) FROM "+f.table("children")).Scan(&children))
	if indexes != 3 || constraints != 4 || children != 3 {
		t.Fatalf("restored metadata indexes=%d constraints=%d children=%d",
			indexes, constraints, children)
	}
	assertRestoreConstraint(t, restored,
		"INSERT INTO "+f.table("children")+" VALUES(999,'x')", "23503")
	assertRestoreConstraint(t, restored, "UPDATE "+f.table("records")+" SET score=-1", "23514")
	var next int
	checkError(t, "verify restored identity sequence", restored.QueryRow(t.Context(),
		"INSERT INTO "+f.table("records")+"(label,score) VALUES('next',101) RETURNING id").Scan(&next))
	if next != 101 {
		t.Fatalf("restored identity sequence = %d, want 101", next)
	}
}

func assertRestoreConstraint(t *testing.T, pool *pgxpool.Pool, sql, state string) {
	_, err := pool.Exec(t.Context(), sql)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state {
		t.Fatalf("restored constraint expected SQLSTATE %s, got %T", state, err)
	}
}
