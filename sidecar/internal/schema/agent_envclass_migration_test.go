package schema

import (
	"strings"
	"testing"
)

// Agent governance G1 (spec §6.5, §6.7, §7): bootstrap creates the
// environment labels and clone receipts, and widens sage.facts to column
// classes keyed by (relid, attnum). Re-running bootstrap changes nothing.

func TestAgentEnvClassMigration_LabelsTable(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // idempotent
	const id = "00000000-0000-4000-8000-0000000000e1"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_environment_labels
			WHERE database_id = $1`, id)
	})
	var label string
	var verified bool
	if err := pool.QueryRow(ctx, `INSERT INTO sage.guard_environment_labels
		(database_id, identity, set_by) VALUES ($1, '{}', 'test')
		RETURNING label, verified`, id).Scan(&label, &verified); err != nil {
		t.Fatalf("insert a default label: %v", err)
	}
	if label != "prod" || verified {
		t.Fatalf("defaults: label %q verified %v, want prod false", label, verified)
	}
	_, err := pool.Exec(ctx, `UPDATE sage.guard_environment_labels SET label = 'qa'
		WHERE database_id = $1`, id)
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("label qa accepted: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE sage.guard_environment_labels
		SET pending_label = 'qa' WHERE database_id = $1`, id)
	if err == nil {
		t.Fatal("pending label qa accepted")
	}
}

func TestAgentEnvClassMigration_CloneReceipts(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.clone_instances WHERE name LIKE 'migtest-%'`)
	})
	insert := `INSERT INTO sage.clone_instances (deployment_id, adapter, scope, name,
		purpose, status, expires_at) VALUES (gen_random_uuid(), 'dle', 's1', $1, $2, $3,
		now() + interval '1 hour')`
	if _, err := pool.Exec(ctx, insert, "migtest-a", "sandbox", "creating"); err != nil {
		t.Fatalf("valid receipt: %v", err)
	}
	for name, args := range map[string][]any{
		"duplicate name": {"migtest-a", "sandbox", "creating"},
		"bad purpose":    {"migtest-b", "fun", "creating"},
		"bad status":     {"migtest-c", "sandbox", "alive"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAgentEnvClassMigration_FactsColumnClass(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.facts WHERE fact_type = 'column_class'`)
	})
	insert := `INSERT INTO sage.facts (fact_type, subject_kind, subject, value, source,
		subject_relid, subject_attnum) VALUES ('column_class', $1, $2,
		'{"class":"pii"}', 'operator', $3, $4)`
	if _, err := pool.Exec(ctx, insert, "column", "16400.2", 16400, 2); err != nil {
		t.Fatalf("column class fact: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "column", "16400.2b", 16400, 2); err == nil {
		t.Error("a second class fact for the same (relid, attnum) was accepted")
	}
	if _, err := pool.Exec(ctx, insert, "column", "16400.0", nil, nil); err == nil {
		t.Error("a column fact without relid/attnum was accepted")
	}
	if _, err := pool.Exec(ctx, insert, "table", "16400", 16400, 0); err != nil {
		t.Errorf("table-level class fact: %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		source) VALUES ('nonsense', 'column', 'x', 'operator')`)
	if err == nil {
		t.Error("unknown fact type accepted after the constraint swap")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.facts'::regclass AND conname IN
		('facts_fact_type_check', 'facts_subject_kind_check')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old inline CHECKs still present: %d %v", n, err)
	}
}
