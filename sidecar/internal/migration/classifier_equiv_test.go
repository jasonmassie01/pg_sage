//go:build cgo

package migration

import (
	"testing"
)

// equivalenceCorpus is every statement of classifier_test.go,
// classifier_regression_test.go and classifier_version_test.go: the
// parse-tree classifier must give the regex classifier's answer on each.
var equivalenceCorpus = []string{
	"CREATE INDEX idx_foo ON bar (col)",
	"CREATE UNIQUE INDEX idx_u ON s.t (c)",
	"CREATE INDEX CONCURRENTLY idx_foo ON bar (col)",
	"create index Idx ON Bar (col)",
	"CREATE INDEX\n  idx_foo\n  ON bar (col)",
	"ALTER TABLE t ADD CONSTRAINT ck_positive CHECK (val > 0)",
	"ALTER TABLE t ADD CONSTRAINT ck CHECK (v > 0) NOT VALID",
	"ALTER TABLE orders ADD CONSTRAINT fk_cust FOREIGN KEY (cust_id) REFERENCES customers(id)",
	"ALTER TABLE orders ADD CONSTRAINT fk_cust FOREIGN KEY (cust_id) REFERENCES customers(id) " +
		"NOT VALID",
	"ALTER TABLE t ALTER COLUMN email SET NOT NULL",
	"ALTER TABLE t ALTER COLUMN email DROP NOT NULL",
	"ALTER TABLE t ALTER COLUMN price TYPE numeric(12,2)",
	"ALTER TABLE t ALTER COLUMN c SET DATA TYPE text",
	"ALTER TABLE t ADD COLUMN status text DEFAULT 'active'",
	"ALTER TABLE t ADD COLUMN id uuid DEFAULT my_custom_func()",
	"ALTER TABLE t ADD COLUMN id uuid DEFAULT gen_random_uuid()",
	"ALTER TABLE t ADD COLUMN created timestamptz DEFAULT now()",
	"ALTER TABLE users DROP COLUMN old_field",
	"ALTER TABLE users DROP COLUMN IF EXISTS old_field",
	"DROP TABLE orders", "DROP TABLE myschema.orders", "DROP TABLE IF EXISTS orders",
	"REINDEX TABLE orders", "REINDEX TABLE CONCURRENTLY orders",
	"VACUUM FULL orders", "VACUUM orders",
	"REFRESH MATERIALIZED VIEW mv_daily_stats",
	"REFRESH MATERIALIZED VIEW CONCURRENTLY mv_daily_stats",
	"CLUSTER orders USING idx_date",
	"ALTER TABLE orders SET TABLESPACE fast_ssd",
	"ALTER TABLE orders ATTACH PARTITION orders_2024 FOR VALUES FROM ('2024-01-01') " +
		"TO ('2025-01-01')",
	"SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN c SET NOT NULL",
	"CREATE INDEX CONCURRENTLY idx ON t (c)",
	"SELECT * FROM orders", "INSERT INTO t VALUES (1)", "UPDATE t SET x = 1",
	"DELETE FROM t WHERE id = 1", "BEGIN", "COMMIT",
	"  CREATE   INDEX   idx_foo   ON   bar  ( col )  ",
	"ALTER TABLE t ADD x int DEFAULT random()", "ALTER TABLE t DROP x",
	"ALTER TABLE t ALTER x TYPE bigint", "ALTER TABLE t ALTER x SET NOT NULL",
	"ALTER TABLE t ADD CHECK (a > 0)", "ALTER TABLE t ADD FOREIGN KEY (a) REFERENCES u (id)",
	`ALTER TABLE "Order Items" ALTER COLUMN "Qty" TYPE bigint`,
	"ALTER TABLE t ADD PRIMARY KEY (id)", "ALTER TABLE t ADD CONSTRAINT u UNIQUE (a)",
	"ALTER TABLE t ADD COLUMN id bigserial",
	"ALTER TABLE t ADD COLUMN id int GENERATED ALWAYS AS IDENTITY",
	"ALTER TABLE t ADD COLUMN s int GENERATED ALWAYS AS (a * 2) STORED",
	"/* deploy 42 */ ALTER TABLE t ALTER COLUMN a TYPE bigint",
	"-- migration\nALTER TABLE t ALTER COLUMN a TYPE bigint",
	"SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN a TYPE bigint",
	"ALTER TABLE t ADD CONSTRAINT u UNIQUE USING INDEX u_idx",
	"ALTER TABLE t ADD COLUMN status text NOT NULL",
	"ALTER TABLE t ADD COLUMN status text NOT NULL DEFAULT 'x'",
	"CLUSTER orders USING orders_pkey", "CREATE INDEX i ON orders (a)", "",
}

// coercionProbes are current column types for the TargetType check: the
// two classifiers may spell a type differently ("bigint" vs "int8"), but
// refineAlterType must reach the same verdict from either spelling.
var coercionProbes = []string{
	"character varying(20)", "character varying", "text", "integer", "numeric(12,2)",
}

func TestASTClassifierMatchesRegexOnExistingCorpus(t *testing.T) {
	re := NewRegexClassifier()
	for _, sql := range equivalenceCorpus {
		for _, v := range []int{0, 140000, 160000, 180000} {
			want := re.Classify(sql, v)
			got := astClassify(sql, v).All()
			if len(got) != len(want) {
				t.Fatalf("%q PG %d: parser %v, regex %v", sql, v, ruleIDs(got), ruleIDs(want))
			}
			for i := range want {
				requireSameClassification(t, sql, got[i], want[i])
			}
		}
	}
}

func requireSameClassification(t *testing.T, sql string, got, want DDLClassification) {
	t.Helper()
	gotType, wantType := got.TargetType, want.TargetType
	got.TargetType, want.TargetType = "", ""
	if got != want {
		t.Fatalf("%q:\nparser %+v\nregex  %+v", sql, got, want)
	}
	if (gotType == "") != (wantType == "") {
		t.Fatalf("%q: target type %q vs %q", sql, gotType, wantType)
	}
	for _, cur := range coercionProbes {
		if isBinaryCoercibleChange(cur, gotType) != isBinaryCoercibleChange(cur, wantType) {
			t.Fatalf("%q: %s -> %q vs %q disagree on the rewrite", sql, cur, gotType, wantType)
		}
	}
}

// The parser and the regex rules agree on the hidden-rewrite spellings,
// and neither names a target type for them (migration-04 review).
func TestASTClassifierMatchesRegexOnHiddenTypeRewrites(t *testing.T) {
	re := NewRegexClassifier()
	for _, sql := range hiddenTypeRewrites {
		want := re.Classify(sql, 170000)
		got := astClassify(sql, 170000).All()
		if len(got) != len(want) || len(got) == 0 {
			t.Fatalf("%q: parser %v, regex %v", sql, ruleIDs(got), ruleIDs(want))
		}
		for i := range want {
			requireSameClassification(t, sql, got[i], want[i])
			if got[i].TargetType != "" {
				t.Fatalf("%q: parser target type %q, want none", sql, got[i].TargetType)
			}
		}
	}
}
