package migration

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schema"
)

// TestClassifier_FalseNegatives is the G7-B26 regression table.
func TestClassifier_FalseNegatives(t *testing.T) {
	cases := []struct{ sql, rule, table string }{
		{"ALTER TABLE t ADD x int DEFAULT random()",
			"ddl_add_column_volatile_default", "t"},
		{"ALTER TABLE t DROP x", "ddl_drop_column", "t"},
		{"ALTER TABLE t ALTER x TYPE bigint", "ddl_alter_type_rewrite", "t"},
		{"ALTER TABLE t ALTER x SET NOT NULL", "ddl_set_not_null", "t"},
		{"ALTER TABLE t ADD CHECK (a > 0)", "ddl_constraint_not_valid", "t"},
		{"ALTER TABLE t ADD FOREIGN KEY (a) REFERENCES u (id)",
			"ddl_fk_not_valid", "t"},
		{`ALTER TABLE "Order Items" ALTER COLUMN "Qty" TYPE bigint`,
			"ddl_alter_type_rewrite", "Order Items"},
		{"ALTER TABLE t ADD PRIMARY KEY (id)", "ddl_add_key_builds_index", "t"},
		{"ALTER TABLE t ADD CONSTRAINT u UNIQUE (a)",
			"ddl_add_key_builds_index", "t"},
		{"ALTER TABLE t ADD COLUMN id bigserial",
			"ddl_add_column_volatile_default", "t"},
		{"ALTER TABLE t ADD COLUMN id int GENERATED ALWAYS AS IDENTITY",
			"ddl_add_column_volatile_default", "t"},
		{"ALTER TABLE t ADD COLUMN s int GENERATED ALWAYS AS (a * 2) STORED",
			"ddl_add_column_volatile_default", "t"},
		{"/* deploy 42 */ ALTER TABLE t ALTER COLUMN a TYPE bigint",
			"ddl_alter_type_rewrite", "t"},
		{"-- migration\nALTER TABLE t ALTER COLUMN a TYPE bigint",
			"ddl_alter_type_rewrite", "t"},
		{"SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN a TYPE bigint",
			"ddl_alter_type_rewrite", "t"},
	}
	c := NewRegexClassifier()
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			got := findRule(c.Classify(tc.sql, 160000), tc.rule)
			if got == nil {
				t.Fatalf("rule %s not detected", tc.rule)
			}
			if got.TableName != tc.table {
				t.Fatalf("table = %q, want %q", got.TableName, tc.table)
			}
		})
	}
}

// TestClassifier_KeyUsingIndexIsSafe: attaching an existing index
// must not be flagged as an index build.
func TestClassifier_KeyUsingIndexIsSafe(t *testing.T) {
	c := NewRegexClassifier()
	got := findRule(c.Classify(
		"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX u_idx", 160000),
		"ddl_add_key_builds_index")
	if got != nil {
		t.Fatal("UNIQUE USING INDEX flagged as index build")
	}
}

// TestClassifier_LockTimeoutInBatchSuppressesAnnotation: a batch that
// sets lock_timeout first must not be told to set it.
func TestClassifier_LockTimeoutInBatchSuppressesAnnotation(t *testing.T) {
	c := NewRegexClassifier()
	got := findRule(c.Classify(
		"SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN a TYPE bigint",
		160000), "ddl_missing_lock_timeout")
	if got != nil {
		t.Fatal("lock_timeout annotation despite SET lock_timeout")
	}
}

// TestIntegration_ResolveStaleMigrationFindings is the G7-B13
// regression: migration_safety findings were never resolved.
func TestIntegration_ResolveStaleMigrationFindings(t *testing.T) {
	pool, ctx := requireDB(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	insert := func(category, ident string, age time.Duration) int64 {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO sage.findings (category, severity, title,
			   detail, object_identifier, last_seen)
			 VALUES ($1, 'warning', 't', '{}', $2, now() - make_interval(secs => $3))
			 RETURNING id`, category, ident,
			age.Seconds()).Scan(&id)
		if err != nil {
			t.Fatalf("insert finding: %v", err)
		}
		return id
	}
	stale := insert(SafetyFindingCategory, "b13:stale", 48*time.Hour)
	fresh := insert(SafetyFindingCategory, "b13:fresh", time.Minute)
	other := insert("index_unused", "b13:other", 48*time.Hour)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.findings WHERE id = ANY($1)`,
			[]int64{stale, fresh, other})
	})

	n, err := ResolveStaleFindings(ctx, pool, 24*time.Hour)
	if err != nil {
		t.Fatalf("ResolveStaleFindings: %v", err)
	}
	if n < 1 {
		t.Fatalf("resolved %d rows, want >= 1", n)
	}
	status := func(id int64) (string, bool) {
		var s string
		var resolvedAt *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT status, resolved_at FROM sage.findings WHERE id = $1`,
			id).Scan(&s, &resolvedAt); err != nil {
			t.Fatalf("read finding: %v", err)
		}
		return s, resolvedAt != nil
	}
	if s, ok := status(stale); s != "resolved" || !ok {
		t.Fatalf("stale finding status=%s resolved_at set=%v", s, ok)
	}
	if s, _ := status(fresh); s != "open" {
		t.Fatalf("fresh finding status=%s, want open", s)
	}
	if s, _ := status(other); s != "open" {
		t.Fatalf("other-category finding status=%s, want open", s)
	}
}
