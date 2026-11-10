package migration

import (
	"context"
	"testing"
)

// Boundary: a role with no search_path setting gets the server default
// ("$user", public), so the role's own schema wins over public, as it
// does in PostgreSQL; a role without such a schema gets public.
func TestAssess_DefaultSearchPathPrefersUserSchema(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS nr_user CASCADE;
			DROP TABLE IF EXISTS public.nr_shared; DROP ROLE IF EXISTS nr_user`)
	})
	mustExecAll(t, ctx, pool,
		"DROP SCHEMA IF EXISTS nr_user CASCADE",
		"DROP TABLE IF EXISTS public.nr_shared",
		"DROP ROLE IF EXISTS nr_user",
		"CREATE ROLE nr_user",
		"CREATE SCHEMA nr_user",
		"CREATE TABLE nr_user.nr_shared (id int)",
		"INSERT INTO nr_user.nr_shared SELECT generate_series(1, 700)",
		"ANALYZE nr_user.nr_shared",
		"CREATE TABLE public.nr_shared (id int)",
		"INSERT INTO public.nr_shared SELECT generate_series(1, 20)",
		"ANALYZE public.nr_shared")
	ra := NewRiskAssessor(pool, func(string, string, ...any) {})
	for role, want := range map[string]struct {
		schema string
		rows   int64
	}{"nr_user": {"nr_user", 700}, "": {"public", 20}} {
		risk, err := ra.Assess(ctx, DDLClassification{
			Statement: "ALTER TABLE nr_shared ADD COLUMN x int",
			LockLevel: "ACCESS EXCLUSIVE", TableName: "nr_shared", SessionRole: role})
		if err != nil {
			t.Fatalf("role %q: Assess: %v", role, err)
		}
		if risk.SchemaName != want.schema || risk.EstimatedRows != want.rows ||
			risk.NameAmbiguous {
			t.Fatalf("role %q: schema %q rows %d ambiguous %v; want %s with %d rows",
				role, risk.SchemaName, risk.EstimatedRows, risk.NameAmbiguous,
				want.schema, want.rows)
		}
	}
}
