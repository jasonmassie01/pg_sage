//go:build integration

package plan

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "migration_plan"))
}

func planDBPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// runPlan executes every planned step in order, expand then contract,
// the way the applier would across two cycles.
func runPlan(t *testing.T, pool *pgxpool.Pool, result Plan) {
	t.Helper()
	for _, step := range stepSQL(result) {
		if _, err := pool.Exec(context.Background(), step); err != nil {
			t.Fatalf("execute planned step %q: %v", step, err)
		}
	}
}

func resetTable(t *testing.T, pool *pgxpool.Pool, ddl string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.mig02_users CASCADE`); err != nil {
		t.Fatalf("drop fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.mig02_users CASCADE`)
	})
}

// TestPlannedUniqueAttachesWithRequestedTiming: the plan for a
// mixed-case, DEFERRABLE request builds, attaches, and the attached
// constraint has exactly the timing the request asked for.
func TestPlannedUniqueAttachesWithRequestedTiming(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	table := TableFacts{Schema: "public", Name: "mig02_users"}
	cases := []struct {
		options              string
		deferrable, deferred bool
	}{
		{"DEFERRABLE INITIALLY DEFERRED", true, true},
		{"DEFERRABLE", true, false},
		{"INITIALLY DEFERRED", true, true},
		{"NOT DEFERRABLE INITIALLY IMMEDIATE", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.options, func(t *testing.T) {
			resetTable(t, pool, `CREATE TABLE public.mig02_users (id int, email text)`)
			result := mustPlan(t, `ALTER TABLE public.mig02_users ADD CONSTRAINT `+
				`Mig02_Email_Key UNIQUE (Email) `+tc.options, table)
			runPlan(t, pool, result)
			var deferrable, deferred bool
			var contype, index string
			err := pool.QueryRow(context.Background(), `
SELECT c.condeferrable, c.condeferred, c.contype::text, c.conindid::regclass::text
FROM pg_constraint c
WHERE c.conrelid = 'public.mig02_users'::regclass AND c.conname = $1`,
				"mig02_email_key").Scan(&deferrable, &deferred, &contype, &index)
			if err != nil {
				t.Fatalf("read attached constraint: %v", err)
			}
			// ADD CONSTRAINT ... USING INDEX renames the index to the constraint.
			if contype != "u" || index != "mig02_email_key" ||
				deferrable != tc.deferrable || deferred != tc.deferred {
				t.Fatalf("constraint = type %s index %s deferrable %v deferred %v, "+
					"want deferrable %v deferred %v", contype, index, deferrable, deferred,
					tc.deferrable, tc.deferred)
			}
		})
	}
}

// TestPlannedUniqueOnQuotedColumnBuilds: a quoted, mixed-case column
// is planned as the real column name, not a tripled-quote name.
func TestPlannedUniqueOnQuotedColumnBuilds(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetTable(t, pool, `CREATE TABLE public.mig02_users (id int, "Email" text)`)
	result := mustPlan(t, `ALTER TABLE public.mig02_users ADD CONSTRAINT "Mig02_Key" `+
		`UNIQUE ("Email")`, TableFacts{Schema: "public", Name: "mig02_users"})
	runPlan(t, pool, result)
	var columns string
	err := pool.QueryRow(context.Background(), `
SELECT string_agg(a.attname, ',')
FROM pg_constraint c JOIN pg_attribute a
  ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
WHERE c.conname = 'Mig02_Key'`).Scan(&columns)
	if err != nil {
		t.Fatalf("read constraint columns: %v", err)
	}
	if columns != "Email" {
		t.Fatalf("constraint columns = %q, want Email", columns)
	}
}

// TestPlannedSetNotNullOnMixedCaseColumn: the expand and contract
// steps for a bare mixed-case column run, the column ends NOT NULL and
// the proof constraint is gone.
func TestPlannedSetNotNullOnMixedCaseColumn(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetTable(t, pool, `CREATE TABLE public.mig02_users (id int, email text);
INSERT INTO public.mig02_users VALUES (1, 'a@example.test')`)
	result := mustPlan(t, `ALTER TABLE public.mig02_users ALTER COLUMN Email SET NOT NULL`,
		TableFacts{Schema: "public", Name: "mig02_users"})
	runPlan(t, pool, result)
	var notNull bool
	var proofs int
	err := pool.QueryRow(context.Background(), `
SELECT a.attnotnull,
       (SELECT count(*) FROM pg_constraint
        WHERE conrelid = a.attrelid AND conname = 'mig02_users_email_nn')
FROM pg_attribute a
WHERE a.attrelid = 'public.mig02_users'::regclass AND a.attname = 'email'`).
		Scan(&notNull, &proofs)
	if err != nil {
		t.Fatalf("read column state: %v", err)
	}
	if !notNull || proofs != 0 {
		t.Fatalf("attnotnull = %v proof constraints = %d, want true and 0", notNull, proofs)
	}
}
