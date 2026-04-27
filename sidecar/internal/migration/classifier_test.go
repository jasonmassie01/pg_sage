package migration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifier_IndexNotConcurrent(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: bare CREATE INDEX", func(t *testing.T) {
		results := c.Classify("CREATE INDEX idx_foo ON bar (col)", 14)
		requireRulePresent(t, results, "ddl_index_not_concurrent")
		assert.Equal(t, "SHARE", results[0].LockLevel)
		assert.Equal(t, "bar", results[0].TableName)
	})

	t.Run("positive: unique index", func(t *testing.T) {
		results := c.Classify("CREATE UNIQUE INDEX idx_u ON s.t (c)", 14)
		requireRulePresent(t, results, "ddl_index_not_concurrent")
		assert.Equal(t, "s", results[0].SchemaName)
		assert.Equal(t, "t", results[0].TableName)
	})

	t.Run("negative: CONCURRENTLY present", func(t *testing.T) {
		results := c.Classify(
			"CREATE INDEX CONCURRENTLY idx_foo ON bar (col)", 14)
		requireRuleAbsent(t, results, "ddl_index_not_concurrent")
	})

	t.Run("positive: mixed case", func(t *testing.T) {
		results := c.Classify("create index Idx ON Bar (col)", 14)
		requireRulePresent(t, results, "ddl_index_not_concurrent")
	})

	t.Run("positive: multi-line", func(t *testing.T) {
		sql := "CREATE INDEX\n  idx_foo\n  ON bar (col)"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_index_not_concurrent")
	})
}

func TestClassifier_ConstraintNotValid(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: CHECK without NOT VALID", func(t *testing.T) {
		sql := "ALTER TABLE t ADD CONSTRAINT ck_positive CHECK (val > 0)"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_constraint_not_valid")
		assert.Equal(t, "ACCESS EXCLUSIVE", results[0].LockLevel)
		assert.Equal(t, "t", results[0].TableName)
	})

	t.Run("negative: CHECK with NOT VALID", func(t *testing.T) {
		sql := "ALTER TABLE t ADD CONSTRAINT ck CHECK (v > 0) NOT VALID"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_constraint_not_valid")
	})
}

func TestClassifier_FKNotValid(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: FK without NOT VALID", func(t *testing.T) {
		sql := "ALTER TABLE orders ADD CONSTRAINT fk_cust " +
			"FOREIGN KEY (cust_id) REFERENCES customers(id)"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_fk_not_valid")
		assert.Equal(t, "SHARE ROW EXCLUSIVE",
			findRule(results, "ddl_fk_not_valid").LockLevel)
	})

	t.Run("negative: FK with NOT VALID", func(t *testing.T) {
		sql := "ALTER TABLE orders ADD CONSTRAINT fk_cust " +
			"FOREIGN KEY (cust_id) REFERENCES customers(id) NOT VALID"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_fk_not_valid")
	})
}

func TestClassifier_SetNotNull(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "ALTER TABLE t ALTER COLUMN email SET NOT NULL"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_set_not_null")
	})

	t.Run("negative: DROP NOT NULL", func(t *testing.T) {
		sql := "ALTER TABLE t ALTER COLUMN email DROP NOT NULL"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_set_not_null")
	})
}

func TestClassifier_AlterTypeRewrite(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: change type", func(t *testing.T) {
		sql := "ALTER TABLE t ALTER COLUMN price TYPE numeric(12,2)"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_alter_type_rewrite")
		r := findRule(results, "ddl_alter_type_rewrite")
		assert.True(t, r.RequiresRewrite)
	})

	t.Run("positive: SET DATA TYPE", func(t *testing.T) {
		sql := "ALTER TABLE t ALTER COLUMN c SET DATA TYPE text"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_alter_type_rewrite")
	})
}

func TestClassifier_AddColumnVolatileDefault(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: volatile on PG10", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN status text DEFAULT 'active'"
		results := c.Classify(sql, 10)
		requireRulePresent(t, results, "ddl_add_column_volatile_default")
	})

	t.Run("negative: safe default on PG14", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN status text DEFAULT 'active'"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_add_column_volatile_default")
	})

	t.Run("positive: volatile func on PG14", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN id uuid DEFAULT my_custom_func()"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_add_column_volatile_default")
	})

	t.Run("negative: allowlisted func gen_random_uuid on PG14", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN id uuid DEFAULT gen_random_uuid()"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_add_column_volatile_default")
	})
}

func TestClassifier_DropColumn(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "ALTER TABLE users DROP COLUMN old_field"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_drop_column")
	})

	t.Run("positive: IF EXISTS", func(t *testing.T) {
		sql := "ALTER TABLE users DROP COLUMN IF EXISTS old_field"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_drop_column")
	})
}

func TestClassifier_DropTable(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "DROP TABLE orders"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_drop_table")
		assert.Equal(t, "orders", findRule(results, "ddl_drop_table").TableName)
	})

	t.Run("positive: schema-qualified", func(t *testing.T) {
		sql := "DROP TABLE myschema.orders"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_drop_table")
		r := findRule(results, "ddl_drop_table")
		assert.Equal(t, "myschema", r.SchemaName)
		assert.Equal(t, "orders", r.TableName)
	})

	t.Run("positive: IF EXISTS", func(t *testing.T) {
		sql := "DROP TABLE IF EXISTS orders"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_drop_table")
	})
}

func TestClassifier_ReindexNotConcurrent(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: PG14", func(t *testing.T) {
		sql := "REINDEX TABLE orders"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_reindex_not_concurrent")
	})

	t.Run("negative: CONCURRENTLY", func(t *testing.T) {
		sql := "REINDEX TABLE CONCURRENTLY orders"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_reindex_not_concurrent")
	})

	t.Run("negative: PG11 (not available)", func(t *testing.T) {
		sql := "REINDEX TABLE orders"
		results := c.Classify(sql, 11)
		requireRuleAbsent(t, results, "ddl_reindex_not_concurrent")
	})
}

func TestClassifier_VacuumFull(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "VACUUM FULL orders"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_vacuum_full")
	})

	t.Run("negative: regular VACUUM", func(t *testing.T) {
		sql := "VACUUM orders"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_vacuum_full")
	})
}

func TestClassifier_RefreshNotConcurrent(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "REFRESH MATERIALIZED VIEW mv_daily_stats"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_refresh_not_concurrent")
	})

	t.Run("negative: CONCURRENTLY", func(t *testing.T) {
		sql := "REFRESH MATERIALIZED VIEW CONCURRENTLY mv_daily_stats"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_refresh_not_concurrent")
	})
}

func TestClassifier_Cluster(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		results := c.Classify("CLUSTER orders USING idx_date", 14)
		requireRulePresent(t, results, "ddl_cluster")
	})
}

func TestClassifier_SetTablespace(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "ALTER TABLE orders SET TABLESPACE fast_ssd"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_set_tablespace")
		assert.Equal(t, "orders",
			findRule(results, "ddl_set_tablespace").TableName)
	})
}

func TestClassifier_AddColumnNotNull(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: PG10 NOT NULL without DEFAULT", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN status text NOT NULL"
		results := c.Classify(sql, 10)
		requireRulePresent(t, results, "ddl_add_column_not_null")
	})

	t.Run("negative: PG14 (safe)", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN status text NOT NULL"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_add_column_not_null")
	})

	t.Run("negative: has DEFAULT", func(t *testing.T) {
		sql := "ALTER TABLE t ADD COLUMN status text NOT NULL DEFAULT 'x'"
		results := c.Classify(sql, 10)
		requireRuleAbsent(t, results, "ddl_add_column_not_null")
	})
}

func TestClassifier_AttachPartition(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive", func(t *testing.T) {
		sql := "ALTER TABLE orders ATTACH PARTITION orders_2024 " +
			"FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_attach_partition_no_check")
	})
}

func TestClassifier_MissingLockTimeout(t *testing.T) {
	c := NewRegexClassifier()

	t.Run("positive: ACCESS EXCLUSIVE without lock_timeout", func(t *testing.T) {
		sql := "ALTER TABLE t ALTER COLUMN c SET NOT NULL"
		results := c.Classify(sql, 14)
		requireRulePresent(t, results, "ddl_missing_lock_timeout")
	})

	t.Run("negative: lock_timeout present", func(t *testing.T) {
		sql := "SET lock_timeout = '5s'; " +
			"ALTER TABLE t ALTER COLUMN c SET NOT NULL"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_missing_lock_timeout")
	})

	t.Run("negative: no ACCESS EXCLUSIVE rules", func(t *testing.T) {
		sql := "CREATE INDEX CONCURRENTLY idx ON t (c)"
		results := c.Classify(sql, 14)
		requireRuleAbsent(t, results, "ddl_missing_lock_timeout")
	})
}

func TestClassifier_NoMatchOnPlainSQL(t *testing.T) {
	c := NewRegexClassifier()
	for _, sql := range []string{
		"SELECT * FROM orders",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET x = 1",
		"DELETE FROM t WHERE id = 1",
		"BEGIN",
		"COMMIT",
	} {
		results := c.Classify(sql, 14)
		assert.Empty(t, results, "should not match: %s", sql)
	}
}

func TestClassifier_ExtraWhitespace(t *testing.T) {
	c := NewRegexClassifier()
	sql := "  CREATE   INDEX   idx_foo   ON   bar  ( col )  "
	results := c.Classify(sql, 14)
	requireRulePresent(t, results, "ddl_index_not_concurrent")
}

// --- test helpers ---

func requireRulePresent(
	t *testing.T, results []DDLClassification, ruleID string,
) {
	t.Helper()
	for _, r := range results {
		if r.RuleID == ruleID {
			return
		}
	}
	var ids []string
	for _, r := range results {
		ids = append(ids, r.RuleID)
	}
	require.Failf(t, "rule not found",
		"expected rule %s in results %v", ruleID, ids)
}

func requireRuleAbsent(
	t *testing.T, results []DDLClassification, ruleID string,
) {
	t.Helper()
	for _, r := range results {
		if r.RuleID == ruleID {
			var ids []string
			for _, r2 := range results {
				ids = append(ids, r2.RuleID)
			}
			require.Failf(t, "unexpected rule",
				"rule %s should not be in results %v", ruleID, ids)
		}
	}
}

func findRule(
	results []DDLClassification, ruleID string,
) *DDLClassification {
	for i := range results {
		if results[i].RuleID == ruleID {
			return &results[i]
		}
	}
	return nil
}
