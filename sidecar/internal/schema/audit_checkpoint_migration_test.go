package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// audit-03: bootstrap creates sage.audit_chain_checkpoint keyed by (chain,
// seq), twice without error, and a second run keeps its rows.
func TestAuditCheckpointMigration(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "schema_audit_checkpoint")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.audit_chain_checkpoint
		(chain, seq, hash, mode) VALUES ('action_log', 4, repeat('a', 64), 'full')`); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap 2: %v", err)
	}
	var key string
	var rows int
	err = pool.QueryRow(ctx, `SELECT pg_get_constraintdef(c.oid),
		(SELECT count(*) FROM sage.audit_chain_checkpoint)
		FROM pg_constraint c WHERE c.conrelid = 'sage.audit_chain_checkpoint'::regclass
		AND c.contype = 'p'`).Scan(&key, &rows)
	if err != nil {
		t.Fatalf("read checkpoint table: %v", err)
	}
	if key != "PRIMARY KEY (chain, seq)" || rows != 1 {
		t.Fatalf("checkpoint table: key %q, %d rows, want PRIMARY KEY (chain, seq) and 1",
			key, rows)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.audit_chain_checkpoint (chain, seq, hash, mode)
		VALUES ('action_log', 5, repeat('b', 64), 'sometimes')`)
	if err == nil {
		t.Fatalf("checkpoint with an unknown mode accepted")
	}
}
