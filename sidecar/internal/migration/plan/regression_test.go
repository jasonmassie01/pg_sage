package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// TestPlannerRejectsMultiClauseAlter is the G7-B24 regression: only
// the first matching clause was planned and the rest silently dropped.
func TestPlannerRejectsMultiClauseAlter(t *testing.T) {
	cases := []string{
		`ALTER TABLE public.users ALTER COLUMN a SET NOT NULL, ` +
			`ALTER COLUMN b SET NOT NULL`,
		`ALTER TABLE public.users ADD CONSTRAINT u UNIQUE (email), ` +
			`ADD COLUMN x int`,
		`ALTER TABLE public.users ALTER COLUMN a SET NOT NULL; DROP TABLE x`,
	}
	for _, sql := range cases {
		_, err := NewPlanner().Plan(context.Background(), Request{
			SQL: sql, Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
		})
		if err == nil || !strings.Contains(err.Error(), "single") {
			t.Fatalf("%q: err = %v, want single-clause rejection", sql, err)
		}
	}
}

// TestPlannerRejectsTableMismatch: the SQL's target table must be the
// table the caller described, or the plan would alter a different one.
func TestPlannerRejectsTableMismatch(t *testing.T) {
	parsetree.Require(t)
	_, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.orders ALTER COLUMN a SET NOT NULL`,
		Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
	})
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("err = %v, want table mismatch naming orders", err)
	}
	// Unqualified, case-folded names resolve against the request.
	_, err = NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE Users ALTER COLUMN a SET NOT NULL`,
		Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil {
		t.Fatalf("unqualified matching table rejected: %v", err)
	}
}

// TestPlannerAllowsCommaInsideUniqueColumns: a column-list comma is
// not a second clause.
func TestPlannerAllowsCommaInsideUniqueColumns(t *testing.T) {
	parsetree.Require(t)
	result, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.users ADD CONSTRAINT u UNIQUE (a, b)`,
		Cycle: 1, Table: TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil || result.Classification != ClassificationAddUnique {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

// TestPlannerGoldenSQLForLowerCaseStatements pins the exact SQL the
// planner emitted for lower-case identifiers before migration-02; the
// identifier reader must leave it byte-identical.
func TestPlannerGoldenSQLForLowerCaseStatements(t *testing.T) {
	parsetree.Require(t)
	const users = `"public"."users"`
	const accounts = `"public"."accounts"`
	uniqueEmail := []string{
		`CREATE UNIQUE INDEX CONCURRENTLY "users_email_key_idx" ON ` + users + ` ("email")`,
		`ALTER TABLE ` + users + ` ADD CONSTRAINT "users_email_key" ` +
			`UNIQUE USING INDEX "users_email_key_idx"`,
	}
	notNull := []string{
		`ALTER TABLE ` + accounts + ` ADD CONSTRAINT "accounts_email_nn" ` +
			`CHECK ("email" IS NOT NULL) NOT VALID`,
		`ALTER TABLE ` + accounts + ` VALIDATE CONSTRAINT "accounts_email_nn"`,
		`ALTER TABLE ` + accounts + ` ALTER COLUMN "email" SET NOT NULL`,
		`ALTER TABLE ` + accounts + ` DROP CONSTRAINT "accounts_email_nn"`,
	}
	cases := []struct {
		sql   string
		table TableFacts
		want  []string
	}{
		{`ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`,
			usersTable(), uniqueEmail},
		{`alter table public.users add constraint users_email_key unique(email);`,
			usersTable(), uniqueEmail},
		{"ALTER TABLE users\n  ADD CONSTRAINT users_email_key\n  UNIQUE ( email )",
			usersTable(), uniqueEmail},
		{`ALTER TABLE public.users ADD CONSTRAINT u UNIQUE (a, b)`, usersTable(),
			[]string{
				`CREATE UNIQUE INDEX CONCURRENTLY "u_idx" ON ` + users + ` ("a", "b")`,
				`ALTER TABLE ` + users + ` ADD CONSTRAINT "u" UNIQUE USING INDEX "u_idx"`,
			}},
		{`ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL`,
			accountsTable(), notNull},
		{`ALTER TABLE ONLY public.accounts ALTER COLUMN email SET NOT NULL;`,
			accountsTable(), notNull},
	}
	for _, tc := range cases {
		got := stepSQL(mustPlan(t, tc.sql, tc.table))
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Fatalf("%q:\n got %q\nwant %q", tc.sql, got, tc.want)
		}
	}
}

// TestPlannerGoldenSQLForProvedNotNull: with a validated proof only the
// SET NOT NULL contract step remains, byte-identical to before.
func TestPlannerGoldenSQLForProvedNotNull(t *testing.T) {
	parsetree.Require(t)
	const want = `ALTER TABLE "public"."accounts" ALTER COLUMN "email" SET NOT NULL`
	proved, err := NewPlanner().Plan(context.Background(), Request{
		SQL: `ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL`, Cycle: 2,
		Table: accountsTable(), Proofs: []Proof{{Kind: ProofValidatedNotNullCheck,
			Column: "email", Constraint: "accounts_email_nn", Validated: true}},
	})
	if err != nil {
		t.Fatalf("Plan with proof: %v", err)
	}
	if got := stepSQL(proved); len(got) != 1 || got[0] != want {
		t.Fatalf("proved plan = %q, want only %q", got, want)
	}
}
