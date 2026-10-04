package schema

import (
	"context"
	"testing"
)

// Roadmap 2.3: sage.facts holds every typed fact of the monitored database,
// one row per (type, subject kind, subject); sage.fact_card_deliveries
// holds the single-use chat tokens of fact cards (token hashes only).
// Additive and idempotent.

func TestFactsMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"facts", "fact_card_deliveries"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, table).Scan(&n); err != nil ||
			n != 1 {
			t.Fatalf("table %s: %d (%v)", table, n, err)
		}
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT i.indisvalid AND i.indisunique FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sage' AND c.relname = 'facts_subject_key'`).Scan(&valid); err != nil ||
		!valid {
		t.Fatalf("facts_subject_key: %v (%v)", valid, err)
	}
}

func TestFactsTableConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.facts WHERE subject LIKE 'mig_%'")
	}
	clean()
	t.Cleanup(clean)
	insert := func(typ, kind, subject, source, status string, decidedBy any) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
			source, status, decided_by, decided_at) VALUES ($1, $2, $3, $4, $5, $6,
			CASE WHEN $6::text IS NULL THEN NULL ELSE now() END)`,
			typ, kind, subject, source, status, decidedBy)
		return err
	}
	if err := insert("test_fixture", "schema", "mig_a_*", "detector", "proposed",
		nil); err != nil {
		t.Fatalf("valid row: %v", err)
	}
	if err := insert("test_fixture", "schema", "mig_a_*", "model", "proposed",
		nil); err == nil {
		t.Fatal("a duplicate (type, kind, subject) was accepted")
	}
	bad := [][]any{
		{"owned_by_magic", "schema", "mig_b", "detector", "proposed", nil},
		{"test_fixture", "view", "mig_c", "detector", "proposed", nil},
		{"test_fixture", "schema", "mig_d", "intern", "proposed", nil},
		{"test_fixture", "schema", "mig_e", "detector", "maybe", nil},
		{"test_fixture", "schema", "mig_f", "detector", "confirmed", nil},
		{"test_fixture", "schema", "", "detector", "proposed", nil},
	}
	for _, row := range bad {
		if err := insert(row[0].(string), row[1].(string), row[2].(string), row[3].(string),
			row[4].(string), row[5]); err == nil {
			t.Fatalf("invalid row accepted: %v", row)
		}
	}
	if err := insert("test_fixture", "schema", "mig_g", "operator", "confirmed",
		"op"); err != nil {
		t.Fatalf("a confirmed row with its decider: %v", err)
	}
}
