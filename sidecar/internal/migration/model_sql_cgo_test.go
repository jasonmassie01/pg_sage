//go:build cgo

package migration

import (
	"context"
	"strings"
	"testing"
)

var orders = modelTarget{Table: "orders"}

var modelDDLCases = []struct {
	name   string
	sql    string
	target modelTarget
	ok     bool
}{
	{"quoted name differs from target", `ALTER TABLE "Orders" ADD COLUMN x int`,
		orders, false},
	{"concurrent index on qualified target",
		"CREATE INDEX CONCURRENTLY orders_x_idx ON public.orders (x)", orders, true},
	{"unique index", "CREATE UNIQUE INDEX CONCURRENTLY u ON orders (x)", orders, true},
	{"grant", "GRANT SELECT ON orders TO PUBLIC", orders, false},
	{"drop", "DROP TABLE orders", orders, false},
	{"other table", "ALTER TABLE customers ADD COLUMN x int", orders, false},
	{"foreign key to another table", "ALTER TABLE orders ADD CONSTRAINT fk " +
		"FOREIGN KEY (c) REFERENCES customers (id) NOT VALID", orders, false},
	{"unqualified when target has a schema", "ALTER TABLE orders ADD COLUMN x int",
		modelTarget{Schema: "app", Table: "orders"}, false},
	{"qualified in target schema", "ALTER TABLE app.orders ADD COLUMN x int",
		modelTarget{Schema: "app", Table: "orders"}, true},
	{"other schema", "ALTER TABLE app.orders ADD COLUMN x int", orders, false},
	{"set lock_timeout", "SET lock_timeout = '5s'", orders, true},
	{"set local statement_timeout", "SET LOCAL statement_timeout = '1min'", orders, true},
	{"set work_mem", "SET work_mem = '1GB'", orders, false},
	{"reset all", "RESET ALL", orders, false},
	{"set transaction", "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE", orders, false},
	{"update target", "UPDATE orders SET x = 0 WHERE x IS NULL", orders, true},
	{"update with writing cte",
		"WITH d AS (DELETE FROM orders RETURNING id) UPDATE orders SET x = 0", orders,
		false},
	{"update calling an admin function",
		"UPDATE orders SET x = pg_terminate_backend(42)", orders, false},
	{"update calling set_config", "UPDATE orders SET s = set_config('a.b', 'c', false)",
		orders, false},
	{"analyze target", "ANALYZE orders", orders, true},
	{"analyze everything", "ANALYZE", orders, false},
	{"vacuum", "VACUUM orders", orders, false},
	{"disable triggers", "ALTER TABLE orders DISABLE TRIGGER ALL", orders, false},
	{"owner change", "ALTER TABLE orders OWNER TO someone", orders, false},
	{"alter index", "ALTER INDEX orders_idx SET (fillfactor = 90)", orders, false},
	{"rename column", "ALTER TABLE orders RENAME COLUMN a TO b", orders, true},
	{"rename table", "ALTER TABLE orders RENAME TO orders_old", orders, false},
	{"do block", "DO $$ BEGIN PERFORM 1; END $$", orders, false},
	{"select", "SELECT pg_sleep(10)", orders, false},
	{"rollback", "ROLLBACK", orders, false},
	{"parse error", "ALTER TABLE orders ADD", orders, false},
}

func TestValidateModelDDLBoundaries(t *testing.T) {
	for _, tc := range modelDDLCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateModelDDL(tc.sql, tc.target)
			if tc.ok && err != nil {
				t.Fatalf("validateModelDDL(%q) = %v, want accepted", tc.sql, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validateModelDDL(%q) accepted, want refused", tc.sql)
			}
		})
	}
}

const typeChangeScript = `-- step 1
SET lock_timeout = '5s';
BEGIN;
ALTER TABLE public.orders ADD COLUMN status_new text;
COMMIT;
WITH batch AS (SELECT id FROM orders WHERE status_new IS NULL LIMIT 1000)
UPDATE orders SET status_new = status::text WHERE id IN (SELECT id FROM batch);
CREATE INDEX CONCURRENTLY orders_status_new_idx ON orders (status_new);
BEGIN;
ALTER TABLE orders DROP COLUMN status;
ALTER TABLE orders RENAME COLUMN status_new TO status;
COMMIT;
ANALYZE orders;
RESET lock_timeout;`

func TestValidateModelDDLAcceptsATypicalScript(t *testing.T) {
	if err := validateModelDDL(typeChangeScript, modelTarget{Table: "orders"}); err != nil {
		t.Fatalf("typical script refused: %v", err)
	}
}

func TestValidateModelDDLNamesTheOffendingStatement(t *testing.T) {
	err := validateModelDDL("SET lock_timeout = '5s';\nGRANT ALL ON orders TO PUBLIC",
		modelTarget{Table: "orders"})
	if err == nil || !strings.Contains(err.Error(), "statement 2") ||
		!strings.Contains(err.Error(), "GrantStmt") {
		t.Fatalf("err = %v, want it to name statement 2 (GrantStmt)", err)
	}
	err = validateModelDDL(`ALTER TABLE "Orders" ADD COLUMN x int`,
		modelTarget{Table: "orders"})
	if err == nil || !strings.Contains(err.Error(), "Orders") {
		t.Fatalf("err = %v, want it to name the foreign relation", err)
	}
}

func TestGenerateScriptKeepsAValidatedScript(t *testing.T) {
	rec := newRecordingLLM(t, "```sql\n"+typeChangeScript+"\n```")
	gen := NewScriptGenerator(rec.client(), nil, 160004, nopMigrationLog)
	got := gen.GenerateScript(context.Background(), ordersRisk())
	if got.SQL != typeChangeScript || got.ModelText != "" || got.Reason != "" {
		t.Fatalf("validated script = %+v, want it as the recommendation", got)
	}
	a := newEgressAdvisor(newRecordingLLM(t, "```sql\n"+typeChangeScript+"\n```"))
	inc := a.buildIncident(context.Background(), ordersRisk())
	if inc.RecommendedSQL != typeChangeScript ||
		chainLink(inc, "model_text_unvalidated") != nil {
		t.Fatalf("incident = %+v, want the validated script and no unvalidated link", inc)
	}
}

const validFallbackReply = `{"lock_level":"ACCESS EXCLUSIVE",` +
	`"requires_rewrite":false,"risk_score":0.9,` +
	`"safe_alternative":"SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN c ` +
	`SET DEFAULT 'y'","explanation":"risky","estimated_duration_seconds":10}`

func TestLLMFallbackKeepsAValidatedAlternative(t *testing.T) {
	rec := newRecordingLLM(t, validFallbackReply)
	inc, err := newEgressAdvisor(rec).Analyze(context.Background(),
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT 'x'")
	if err != nil || inc == nil {
		t.Fatalf("Analyze = %v, %v", inc, err)
	}
	want := "SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN c SET DEFAULT 'y'"
	if inc.RecommendedSQL != want {
		t.Errorf("RecommendedSQL = %q, want %q", inc.RecommendedSQL, want)
	}
	if l := chainLink(inc, "safe_alternative"); l == nil || l.Description != want {
		t.Errorf("safe_alternative link = %+v", l)
	}
	if chainLink(inc, "model_text_unvalidated") != nil {
		t.Error("validated alternative also labelled unvalidated")
	}
}
