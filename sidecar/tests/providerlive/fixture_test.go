//go:build providerlive

package providerlive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

type fixture struct {
	pool      *pgxpool.Pool
	namespace string
	provider  string
}

func newFixture(t *testing.T, pool *pgxpool.Pool, provider target) fixture {
	t.Helper()
	f := newNamespaceFixture(t, pool, provider)
	// CREATE without IF NOT EXISTS refuses existing metadata, including another live run.
	createOwnedSchema(t, pool, "sage", f.namespace)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	checkError(t, "bootstrap disposable metadata", schema.Bootstrap(ctx, pool))
	return f
}

func newNamespaceFixture(t *testing.T, pool *pgxpool.Pool, provider target) fixture {
	t.Helper()
	requireMutationTarget(t, pool, provider)
	var nonce [12]byte
	_, err := rand.Read(nonce[:])
	checkError(t, "generate fixture identity", err)
	f := fixture{pool: pool, namespace: "sage_live_" + hex.EncodeToString(nonce[:]),
		provider: provider.name}
	createOwnedSchema(t, pool, f.namespace, f.namespace)
	return f
}

func createOwnedSchema(t *testing.T, pool *pgxpool.Pool, name, marker string) {
	t.Helper()
	quoted := pgx.Identifier{name}.Sanitize()
	_, err := pool.Exec(t.Context(), "CREATE SCHEMA "+quoted)
	checkError(t, "create exclusive fixture schema", err)
	var oid uint32
	checkError(t, "record fixture schema identity", pool.QueryRow(t.Context(),
		"SELECT oid FROM pg_catalog.pg_namespace WHERE nspname=$1", name).Scan(&oid))
	// A random locally generated marker is a literal; neither identifiers nor data come from users.
	_, err = pool.Exec(t.Context(), "COMMENT ON SCHEMA "+quoted+" IS '"+marker+"'")
	checkError(t, "mark fixture schema ownership", err)
	t.Cleanup(func() { cleanupSchema(t, pool, name, marker, oid) })
}

func cleanupSchema(t *testing.T, pool *pgxpool.Pool, name, marker string, oid uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// This fixture requires an exclusive disposable project. Identity checks detect accidental
	// reuse; they do not protect against another actor concurrently replacing a schema.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Errorf("cleanup could not begin: %T; fixture retained", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var currentOID uint32
	var comment string
	err = tx.QueryRow(ctx, `SELECT oid, coalesce(obj_description(oid,'pg_namespace'),'')
		FROM pg_catalog.pg_namespace WHERE nspname=$1`, name).Scan(&currentOID, &comment)
	if err != nil || oid != currentOID || comment != marker {
		t.Error("cleanup refused: fixture schema ownership changed; inspect disposable project")
		return
	}
	_, err = tx.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE")
	if err != nil {
		t.Errorf("cleanup DROP failed: %T; fixture retained", err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		t.Errorf("cleanup commit failed: %T", err)
	}
}

func (f fixture) table(name string) string {
	return pgx.Identifier{f.namespace, name}.Sanitize()
}

func (f fixture) exec(t *testing.T, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := f.pool.Exec(ctx, sql)
	checkError(t, "execute synthetic fixture SQL", err)
}
