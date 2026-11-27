//go:build integration

package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

func execAll(t *testing.T, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, sql := range stmts {
		if _, err := pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("fixture %q: %v", sql, err)
		}
	}
}

// swapSchema recreates schema swap09b with cleanup.
func swapSchema(t *testing.T, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	execAll(t, pool, `DROP SCHEMA IF EXISTS swap09b CASCADE`, `CREATE SCHEMA swap09b`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS swap09b CASCADE`)
	})
	execAll(t, pool, stmts...)
}

func readFacts(t *testing.T, pool *pgxpool.Pool, table, column string) (SwapFacts, error) {
	t.Helper()
	reader := catalogread.New(pool, catalogread.Default())
	return ReadSwapFacts(context.Background(), reader, "swap09b", table, column)
}

func TestReadSwapFactsPrimaryKeyWithSequenceAndFK(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool,
		`CREATE TABLE swap09b.orders (id serial PRIMARY KEY, v text)`,
		`CREATE TABLE swap09b.lines (id serial PRIMARY KEY,
			order_id int REFERENCES swap09b.orders (id) ON DELETE CASCADE)`)
	facts, err := readFacts(t, pool, "orders", "id")
	if err != nil {
		t.Fatalf("ReadSwapFacts: %v", err)
	}
	c := facts.Column
	if facts.RelKind != "r" || c.Type != "integer" || !c.NotNull || !c.DefaultIsSeq ||
		c.SequenceSchema != "swap09b" || c.SequenceName != "orders_id_seq" ||
		c.SequenceType != "integer" || c.Identity || c.Generated {
		t.Fatalf("column facts %+v (relkind %q)", c, facts.RelKind)
	}
	if facts.PrimaryKey == nil || facts.PrimaryKey.Constraint != "orders_pkey" ||
		strings.Join(facts.PrimaryKey.Columns, ",") != "id" ||
		facts.PrimaryKey.ColumnType != "integer" {
		t.Fatalf("primary key %+v", facts.PrimaryKey)
	}
	if len(facts.Referencing) != 1 {
		t.Fatalf("referencing %+v", facts.Referencing)
	}
	fk := facts.Referencing[0]
	if fk.Constraint != "lines_order_id_fkey" || fk.Table != "lines" || fk.Column != "order_id" ||
		fk.ColumnType != "integer" || fk.OnDelete != "c" || fk.OnUpdate != "a" ||
		fk.Columns != 1 || fk.ChildPartitioned || fk.ChildIsPartition {
		t.Fatalf("foreign key %+v", fk)
	}
	if len(facts.Dependents) != 0 || len(facts.Outgoing) != 0 || facts.ServerVersion < 140000 {
		t.Fatalf("dependents %v outgoing %v version %d", facts.Dependents, facts.Outgoing,
			facts.ServerVersion)
	}
	if _, err := PlanColumnSwap(context.Background(), facts, SwapRequest{Schema: "swap09b",
		Table: "orders", Column: "id", TargetType: "bigint"}); err != nil {
		t.Fatalf("plan from live facts: %v", err)
	}
}

// Error propagation: a missing table or column names what was not found.
func TestReadSwapFactsMissingObjects(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool, `CREATE TABLE swap09b.t (id int PRIMARY KEY, a text)`)
	if _, err := readFacts(t, pool, "nope", "a"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing table: err = %v", err)
	}
	if _, err := readFacts(t, pool, "t", "nope"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing column: err = %v", err)
	}
}

// Invalid input, live: a table without a primary key is refused.
func TestColumnSwapLiveRefusesNoPrimaryKey(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool, `CREATE TABLE swap09b.t (id int, a text)`)
	facts, err := readFacts(t, pool, "t", "a")
	if err != nil {
		t.Fatalf("ReadSwapFacts: %v", err)
	}
	refuse(t, facts, SwapRequest{Schema: "swap09b", Table: "t", Column: "a",
		TargetType: "bigint"}, "no primary key")
}

// Boundary, live: a key referenced by a partitioned table's foreign key
// is refused as out of scope.
func TestColumnSwapLiveRefusesPartitionedReferencingTable(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool,
		`CREATE TABLE swap09b.t (id int PRIMARY KEY)`,
		`CREATE TABLE swap09b.p (k int, t_id int REFERENCES swap09b.t (id))
			PARTITION BY RANGE (k)`,
		`CREATE TABLE swap09b.p1 PARTITION OF swap09b.p FOR VALUES FROM (0) TO (10)`)
	facts, err := readFacts(t, pool, "t", "id")
	if err != nil {
		t.Fatalf("ReadSwapFacts: %v", err)
	}
	refuse(t, facts, SwapRequest{Schema: "swap09b", Table: "t", Column: "id",
		TargetType: "bigint"}, "out of scope")
}

// Objects the swap would orphan (an index, a view, a CHECK) are named in
// the refusal instead of being silently dropped with the old column.
func TestColumnSwapLiveRefusesDependents(t *testing.T) {
	pool := planDBPool(t)
	for name, ddl := range map[string]string{
		"index": `CREATE INDEX t_a_idx ON swap09b.t (a)`,
		"view":  `CREATE VIEW swap09b.v AS SELECT a FROM swap09b.t`,
		"check": `ALTER TABLE swap09b.t ADD CONSTRAINT a_chk CHECK (a <> '')`,
	} {
		swapSchema(t, pool, `CREATE TABLE swap09b.t (id int PRIMARY KEY, a text)`, ddl)
		facts, err := readFacts(t, pool, "t", "a")
		if err != nil {
			t.Fatalf("%s: ReadSwapFacts: %v", name, err)
		}
		if len(facts.Dependents) == 0 {
			t.Fatalf("%s: no dependents read", name)
		}
		refuse(t, facts, SwapRequest{Schema: "swap09b", Table: "t", Column: "a",
			TargetType: "bigint"}, "depends on")
	}
}

// The facts reads are bounded: they run in catalogread's read-only
// transactions, so a write in one fails.
func TestReadSwapFactsRunsReadOnly(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool, `CREATE TABLE swap09b.t (id int PRIMARY KEY, a text)`)
	reader := catalogread.New(pool, catalogread.Default())
	var readOnly string
	if err := reader.QueryRow(context.Background(),
		`SELECT current_setting('transaction_read_only')`).Scan(&readOnly); err != nil ||
		readOnly != "on" {
		t.Fatalf("reader transaction_read_only = %q, %v", readOnly, err)
	}
	if _, err := readFacts(t, pool, "t", "a"); err != nil {
		t.Fatalf("ReadSwapFacts through the bounded reader: %v", err)
	}
}
