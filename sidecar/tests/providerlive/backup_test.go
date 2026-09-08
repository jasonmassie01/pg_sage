//go:build providerlive

package providerlive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Restores real archives into explicitly designated, fresh local databases.
// No parallel access: each source schema and restore database has exclusive ownership.
func TestProviderLogicalBackupRestore(t *testing.T) {
	dump, restore := backupBinary(t, "SAGE_PGDUMP_BIN", "pg_dump"),
		backupBinary(t, "SAGE_PGRESTORE_BIN", "pg_restore")
	for _, provider := range []target{{"neon", "SAGE_NEON"}, {"supabase", "SAGE_SUPABASE"}} {
		t.Run(provider.name, func(t *testing.T) {
			dsn := os.Getenv(provider.prefix + "_DATABASE_URL")
			if dsn == "" {
				t.Skip(provider.prefix + "_DATABASE_URL absent; backup NOT verified")
			}
			pool := connect(t, dsn)
			f := newNamespaceFixture(t, pool, provider)
			extension := seedBackupFixture(t, f)
			restored := newRestoreDatabase(t, f.namespace, extension)
			archive := filepath.Join(t.TempDir(), "synthetic.dump")
			runBackupTool(t, dump, pool, "--format=custom", "--no-owner", "--no-acl",
				"--schema="+f.namespace, "--file="+archive)
			info, err := os.Stat(archive)
			checkError(t, "inspect real backup archive", err)
			if info.Size() < 100 {
				t.Fatal("archive is empty")
			}
			runBackupTool(t, restore, restored, "--no-owner", "--no-acl", "--exit-on-error",
				"--single-transaction", "--dbname="+restored.Config().ConnConfig.Database, archive)
			verifyRestoredFixture(t, f, restored)
		})
	}
}

func seedBackupFixture(t *testing.T, f fixture) string {
	var ext string
	checkError(t, "find source vector namespace", f.pool.QueryRow(t.Context(),
		`SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace
		WHERE e.extname='vector'`).Scan(&ext))
	vector := pgx.Identifier{ext, "vector"}.Sanitize()
	f.exec(t, "CREATE TABLE "+f.table("records")+" (id bigint GENERATED ALWAYS AS IDENTITY "+
		"PRIMARY KEY, label text NOT NULL UNIQUE, score int CHECK(score>0), embedding "+vector+"(2))")
	f.exec(t, "INSERT INTO "+f.table("records")+"(label,score,embedding) "+
		"SELECT 'synthetic-'||i, i, ('['||i||',0]')::"+vector+" FROM generate_series(1,100) i")
	f.exec(t, "CREATE INDEX backup_hnsw ON "+f.table("records")+" USING hnsw(embedding "+
		pgx.Identifier{ext, "vector_l2_ops"}.Sanitize()+")")
	f.exec(t, "CREATE TABLE "+f.table("children")+"(parent_id bigint REFERENCES "+
		f.table("records")+"(id), payload text)")
	f.exec(t, "INSERT INTO "+f.table("children")+" VALUES (1,'a'),(2,'b'),(3,'c')")
	return ext
}

func newRestoreDatabase(t *testing.T, name, extension string) *pgxpool.Pool {
	dsn := os.Getenv("SAGE_PROVIDER_RESTORE_DATABASE_URL")
	if dsn == "" {
		t.Fatal("explicit local SAGE_PROVIDER_RESTORE_DATABASE_URL required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	checkError(t, "parse restore target", err)
	if (cfg.ConnConfig.Host != "127.0.0.1" && cfg.ConnConfig.Host != "localhost") ||
		cfg.ConnConfig.Database != "postgres" {
		t.Fatal("restore requires a local admin fixture")
	}
	admin := connect(t, dsn)
	quoted := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+quoted)
	checkError(t, "create exclusive restore database", err)
	var oid uint32
	checkError(t, "record restore database identity", admin.QueryRow(t.Context(),
		"SELECT oid FROM pg_database WHERE datname=$1", name).Scan(&oid))
	t.Cleanup(func() { cleanupRestoreDatabase(t, admin, name, oid) })
	cfg.ConnConfig.Database = name
	restored, err := pgxpool.NewWithConfig(t.Context(), cfg)
	checkError(t, "open restore database", err)
	t.Cleanup(restored.Close)
	_, err = restored.Exec(t.Context(), "CREATE SCHEMA IF NOT EXISTS "+
		pgx.Identifier{extension}.Sanitize())
	checkError(t, "create vector dependency namespace", err)
	_, err = restored.Exec(t.Context(), "CREATE EXTENSION vector WITH SCHEMA "+
		pgx.Identifier{extension}.Sanitize())
	checkError(t, "install restore vector dependency", err)
	return restored
}

func cleanupRestoreDatabase(t *testing.T, admin *pgxpool.Pool, name string, oid uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var current uint32
	err := admin.QueryRow(ctx, "SELECT oid FROM pg_database WHERE datname=$1", name).Scan(&current)
	if err != nil || current != oid {
		t.Error("restore cleanup ownership changed")
		return
	}
	_, err = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
	if err != nil {
		t.Errorf("restore cleanup failed: %T", err)
	}
}
