//go:build integration

package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// Integration (migration-06): the rendered forward-fix script runs as
// written on PostgreSQL, and its header stops it, with nothing changed,
// when the expand phase's proof does not hold.

const contractFixture = `CREATE TABLE public.mig06_users (id bigint PRIMARY KEY, email text)`

func contractTable() TableFacts { return TableFacts{Schema: "public", Name: "mig06_users"} }

func resetContractTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	drop := `DROP TABLE IF EXISTS public.mig06_users CASCADE`
	if _, err := pool.Exec(ctx, drop+"; "+contractFixture+
		"; INSERT INTO public.mig06_users VALUES (1, 'a@example.invalid')"); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), drop) })
}

func renderScript(t *testing.T, sql string) (Plan, string) {
	t.Helper()
	planned, err := NewPlanner().Plan(context.Background(),
		Request{SQL: sql, Table: contractTable()})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	script, err := planned.ContractScript()
	if err != nil {
		t.Fatalf("ContractScript: %v", err)
	}
	return planned, script
}

func execExpandSteps(t *testing.T, pool *pgxpool.Pool, steps []Step) {
	t.Helper()
	for _, step := range steps {
		if _, err := pool.Exec(context.Background(), step.SQL); err != nil {
			t.Fatalf("expand step %s: %v", step.Kind, err)
		}
	}
}

func columnNullable(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var notNull bool
	if err := pool.QueryRow(context.Background(), `SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'public.mig06_users'::regclass AND attname = 'email'`).
		Scan(&notNull); err != nil {
		t.Fatalf("read attnotnull: %v", err)
	}
	return !notNull
}

func TestContractScriptRunsAfterExpand(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetContractTable(t, pool)
	planned, script := renderScript(t,
		`ALTER TABLE public.mig06_users ALTER COLUMN email SET NOT NULL`)
	execExpandSteps(t, pool, planned.ExpandSteps)
	if _, err := pool.Exec(context.Background(), script); err != nil {
		t.Fatalf("contract script failed after a validated expand: %v\n%s", err, script)
	}
	var proofLeft bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1
		FROM pg_constraint WHERE conname = 'mig06_users_email_nn')`).Scan(&proofLeft); err != nil {
		t.Fatalf("read proof constraint: %v", err)
	}
	if columnNullable(t, pool) || proofLeft {
		t.Fatalf("after script: nullable=%v proof constraint left=%v",
			columnNullable(t, pool), proofLeft)
	}
}

// The header raises when the CHECK was added NOT VALID but never
// validated, and the transaction leaves the column as it was.
func TestContractScriptStopsOnUnvalidatedProof(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetContractTable(t, pool)
	planned, script := renderScript(t,
		`ALTER TABLE public.mig06_users ALTER COLUMN email SET NOT NULL`)
	execExpandSteps(t, pool, planned.ExpandSteps[:1]) // NOT VALID only
	_, err := pool.Exec(context.Background(), script)
	if err == nil || !strings.Contains(err.Error(), "mig06_users_email_nn") {
		t.Fatalf("script error = %v, want the guard naming the proof", err)
	}
	if !columnNullable(t, pool) {
		t.Fatal("the column became NOT NULL although the guard raised")
	}
	var one int
	if err := pool.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil ||
		one != 1 {
		t.Fatalf("connection unusable after the guard raised: %v", err)
	}
}

func TestContractScriptUniqueNeedsValidIndex(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetContractTable(t, pool)
	planned, script := renderScript(t,
		`ALTER TABLE public.mig06_users ADD CONSTRAINT mig06_email_key UNIQUE (email)`)
	if _, err := pool.Exec(context.Background(), script); err == nil {
		t.Fatal("script ran without the expand index")
	}
	execExpandSteps(t, pool, planned.ExpandSteps)
	if _, err := pool.Exec(context.Background(), script); err != nil {
		t.Fatalf("script after a valid index: %v\n%s", err, script)
	}
	var attached bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1
		FROM pg_constraint WHERE conname = 'mig06_email_key' AND contype = 'u')`).
		Scan(&attached); err != nil || !attached {
		t.Fatalf("unique constraint attached=%v err=%v", attached, err)
	}
}
