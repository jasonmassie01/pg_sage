package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Decision ledger dedupe (dogfood lifeos): non-execute verdicts carry a
// fingerprint and repeats update one row (repeat_count, last_seen_at). The
// unique index behind the upsert, and the ledger's performance indexes,
// are created by Bootstrap, so the upsert works from the first decision.

func TestDecisionLedgerMigrationAddsFingerprintColumnsAndUniqueIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	columns := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT column_name, data_type || ':' || is_nullable ||
		':' || COALESCE(column_default, '') FROM information_schema.columns
		WHERE table_schema='sage' AND table_name='decision'
		  AND column_name IN ('fingerprint', 'repeat_count', 'last_seen_at')`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns[name] = kind
	}
	rows.Close()
	if columns["fingerprint"] != "text:YES:" || columns["repeat_count"] != "integer:NO:1" ||
		columns["last_seen_at"] != "timestamp with time zone:YES:" {
		t.Fatalf("decision columns = %v", columns)
	}
	def := indexDefinition(t, pool, "idx_decision_fingerprint")
	if !strings.Contains(def, "UNIQUE INDEX") || !strings.Contains(def, "(fingerprint)") ||
		!strings.Contains(def, "fingerprint IS NOT NULL") ||
		!strings.Contains(def, "resolved_at IS NULL") {
		t.Fatalf("idx_decision_fingerprint = %s, want a unique partial index on open "+
			"fingerprints", def)
	}
	registered := false
	for _, statement := range migrationStatements() {
		registered = registered || strings.Contains(statement, "idx_decision_fingerprint")
	}
	if !registered {
		t.Fatal("the fingerprint index is not a registered migration")
	}
}

func indexDefinition(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(), `SELECT pg_get_indexdef(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sage' AND c.relname = $1`, name).Scan(&def); err != nil {
		t.Fatalf("index %s: %v", name, err)
	}
	return def
}

// Every foreign key into or out of sage.decision leads some valid index, so
// retention's purges and the anti-joins in its keep rules never scan the
// table; created_at leads an index for the age-based purge.
func TestBootstrapIndexesEveryDecisionForeignKey(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT c.conrelid::regclass::text, a.attname
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND cardinality(c.conkey) = 1
		  AND (c.confrelid = 'sage.decision'::regclass OR c.conrelid = 'sage.decision'::regclass)
		  AND NOT EXISTS (SELECT 1 FROM pg_index i
		                  WHERE i.indrelid = c.conrelid AND i.indisvalid
		                    AND i.indkey[0] = c.conkey[1])
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		missing = append(missing, table+"."+column)
	}
	if len(missing) > 0 {
		t.Fatalf("foreign keys without a leading index: %v", missing)
	}
	if def := indexDefinition(t, pool, "idx_decision_created"); !strings.Contains(def,
		"(created_at)") {
		t.Fatalf("idx_decision_created = %s", def)
	}
}
