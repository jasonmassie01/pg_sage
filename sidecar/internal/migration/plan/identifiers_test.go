package plan

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// migration-02: the planner folds and unquotes identifiers the way
// PostgreSQL does, quotes each emitted name exactly once, and refuses
// any option it cannot carry into the rewritten plan.

func usersTable() TableFacts { return TableFacts{Schema: "public", Name: "users"} }

func accountsTable() TableFacts { return TableFacts{Schema: "public", Name: "accounts"} }

func mustPlan(t *testing.T, sql string, table TableFacts) Plan {
	t.Helper()
	result, err := NewPlanner().Plan(context.Background(),
		Request{SQL: sql, Cycle: 3, Table: table})
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	return result
}

func refusePlan(t *testing.T, sql string, table TableFacts, fragments ...string) {
	t.Helper()
	result, err := NewPlanner().Plan(context.Background(),
		Request{SQL: sql, Cycle: 3, Table: table})
	if err == nil {
		t.Fatalf("Plan(%q) = %#v, want refusal", sql, result)
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("Plan(%q) error = %q, want it to contain %q", sql, err, fragment)
		}
	}
	if len(result.ExpandSteps) != 0 || len(result.ContractSteps) != 0 ||
		result.Classification != "" {
		t.Fatalf("Plan(%q) refused but returned a plan %#v", sql, result)
	}
}

func uniqueSQL(t *testing.T, sql string) (expand, contract string) {
	t.Helper()
	result := mustPlan(t, sql, usersTable())
	if result.Classification != ClassificationAddUnique ||
		len(result.ExpandSteps) != 1 || len(result.ContractSteps) != 1 {
		t.Fatalf("Plan(%q) = %#v, want one expand and one contract step", sql, result)
	}
	return result.ExpandSteps[0].SQL, result.ContractSteps[0].SQL
}

func TestPlannerUniqueFoldsAndUnquotesIdentifiers(t *testing.T) {
	parsetree.Require(t)
	cases := []struct{ name, sql, expand, contract string }{
		{"quoted column",
			`ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE ("email")`,
			`CREATE UNIQUE INDEX CONCURRENTLY "users_email_key_idx" ON "public"."users" ("email")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" ` +
				`UNIQUE USING INDEX "users_email_key_idx"`},
		{"mixed-case bare names",
			`ALTER TABLE public.users ADD CONSTRAINT Users_Email_Key UNIQUE (Email)`,
			`CREATE UNIQUE INDEX CONCURRENTLY "users_email_key_idx" ON "public"."users" ("email")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" ` +
				`UNIQUE USING INDEX "users_email_key_idx"`},
		{"quoted constraint and mixed columns",
			`ALTER TABLE public.users ADD CONSTRAINT "Users_Email_Key" UNIQUE ("Email", Name)`,
			`CREATE UNIQUE INDEX CONCURRENTLY "Users_Email_Key_idx" ON "public"."users" ` +
				`("Email", "name")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "Users_Email_Key" ` +
				`UNIQUE USING INDEX "Users_Email_Key_idx"`},
		{"embedded double quote",
			`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE ("we""ird")`,
			`CREATE UNIQUE INDEX CONCURRENTLY "k_idx" ON "public"."users" ("we""ird")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "k" UNIQUE USING INDEX "k_idx"`},
		{"quoted name keeps its spaces",
			`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (" email")`,
			`CREATE UNIQUE INDEX CONCURRENTLY "k_idx" ON "public"."users" (" email")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "k" UNIQUE USING INDEX "k_idx"`},
		{"quoted keyword-like column",
			`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE ("UNIQUE")`,
			`CREATE UNIQUE INDEX CONCURRENTLY "k_idx" ON "public"."users" ("UNIQUE")`,
			`ALTER TABLE "public"."users" ADD CONSTRAINT "k" UNIQUE USING INDEX "k_idx"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expand, contract := uniqueSQL(t, tc.sql)
			if expand != tc.expand {
				t.Fatalf("expand SQL\n got %s\nwant %s", expand, tc.expand)
			}
			if contract != tc.contract {
				t.Fatalf("contract SQL\n got %s\nwant %s", contract, tc.contract)
			}
		})
	}
}

func TestPlannerSetNotNullFoldsColumnAndProofName(t *testing.T) {
	parsetree.Require(t)
	cases := []struct{ name, sql, column, proof string }{
		{"mixed-case bare", `ALTER TABLE public.accounts ALTER COLUMN Email SET NOT NULL`,
			`"email"`, `"accounts_email_nn"`},
		{"quoted", `ALTER TABLE public.accounts ALTER COLUMN "Email" SET NOT NULL`,
			`"Email"`, `"accounts_Email_nn"`},
		{"lower-case keywords", `alter table public.accounts alter column EMAIL set not null`,
			`"email"`, `"accounts_email_nn"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := mustPlan(t, tc.sql, accountsTable())
			want := []string{
				`ALTER TABLE "public"."accounts" ADD CONSTRAINT ` + tc.proof +
					` CHECK (` + tc.column + ` IS NOT NULL) NOT VALID`,
				`ALTER TABLE "public"."accounts" VALIDATE CONSTRAINT ` + tc.proof,
				`ALTER TABLE "public"."accounts" ALTER COLUMN ` + tc.column + ` SET NOT NULL`,
				`ALTER TABLE "public"."accounts" DROP CONSTRAINT ` + tc.proof,
			}
			got := stepSQL(result)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("steps\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestPlannerNotNullProofMatchesFoldedColumnExactly(t *testing.T) {
	parsetree.Require(t)
	plan := func(sql, proofColumn string) Plan {
		result, err := NewPlanner().Plan(context.Background(), Request{
			SQL: sql, Cycle: 5, Table: accountsTable(),
			Proofs: []Proof{{Kind: ProofValidatedNotNullCheck, Column: proofColumn,
				Constraint: "p", Validated: true}},
		})
		if err != nil {
			t.Fatalf("Plan(%q): %v", sql, err)
		}
		return result
	}
	// A bare Email folds to email, which the catalog proof names.
	if got := plan(`ALTER TABLE public.accounts ALTER COLUMN Email SET NOT NULL`,
		"email"); got.Rewritten || len(got.ExpandSteps) != 0 {
		t.Fatalf("proof for email ignored for bare Email: %#v", got)
	}
	// A proof on column "Email" says nothing about column email.
	if got := plan(`ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL`,
		"Email"); !got.Rewritten || len(got.ExpandSteps) != 2 {
		t.Fatalf("proof on \"Email\" accepted for email: %#v", got)
	}
}

func TestPlannerCarriesDeferrableIntoAttachStep(t *testing.T) {
	parsetree.Require(t)
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email) `
	const attach = `ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" ` +
		`UNIQUE USING INDEX "users_email_key_idx"`
	const index = `CREATE UNIQUE INDEX CONCURRENTLY "users_email_key_idx" ` +
		`ON "public"."users" ("email")`
	cases := map[string]string{
		"DEFERRABLE INITIALLY DEFERRED":       " DEFERRABLE INITIALLY DEFERRED",
		"initially deferred deferrable":       " DEFERRABLE INITIALLY DEFERRED",
		"DEFERRABLE":                          " DEFERRABLE",
		"NOT DEFERRABLE":                      " NOT DEFERRABLE",
		"INITIALLY DEFERRED":                  " INITIALLY DEFERRED",
		"Deferrable Initially Immediate":      " DEFERRABLE INITIALLY IMMEDIATE",
		"NOT DEFERRABLE INITIALLY IMMEDIATE;": " NOT DEFERRABLE INITIALLY IMMEDIATE",
	}
	for options, suffix := range cases {
		t.Run(options, func(t *testing.T) {
			expand, contract := uniqueSQL(t, prefix+options)
			if expand != index {
				t.Fatalf("expand SQL changed by %q: %s", options, expand)
			}
			if contract != attach+suffix {
				t.Fatalf("attach SQL\n got %s\nwant %s", contract, attach+suffix)
			}
		})
	}
}

func TestPlannerRefusesConflictingConstraintTiming(t *testing.T) {
	parsetree.Require(t)
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (email) `
	for _, options := range []string{
		"NOT DEFERRABLE INITIALLY DEFERRED",
		"INITIALLY DEFERRED NOT DEFERRABLE",
		"DEFERRABLE DEFERRABLE",
		"DEFERRABLE NOT DEFERRABLE",
		"INITIALLY DEFERRED INITIALLY IMMEDIATE",
	} {
		refusePlan(t, prefix+options, usersTable(), "plan it by hand")
	}
	// PostgreSQL's parser rejects these itself (migration-02b).
	for _, options := range []string{"INITIALLY LATER", "INITIALLY", "NOT VALID", "NOT"} {
		refusePlan(t, prefix+options, usersTable(), "cannot parse", "plan it by hand")
	}
}

func TestPlannerRefusesOptionsItCannotCarry(t *testing.T) {
	parsetree.Require(t)
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE `
	cases := map[string]string{
		"(email) INCLUDE (name)":              `"INCLUDE (name)"`,
		"(email) WITH (fillfactor=70)":        `"WITH (fillfactor=70)"`,
		"(email) USING INDEX TABLESPACE fast": `"USING INDEX TABLESPACE fast"`,
		"NULLS NOT DISTINCT (email)":          `"NULLS NOT DISTINCT (email)"`,
		"NULLS DISTINCT (email)":              `"NULLS DISTINCT (email)"`,
	}
	for tail, option := range cases {
		refusePlan(t, prefix+tail, usersTable(),
			"unsupported option "+option, "plan it by hand")
	}
	// Text PostgreSQL's own parser rejects is refused at the place it
	// stops reading (migration-02b).
	unparsable := map[string]string{
		"(email) DEFERRABLE INCLUDE (name)":       `at "INCLUDE (name)"`,
		"(email) garbage":                         `at "garbage"`,
		"(email) 'literal'":                       `at "'literal'"`,
		"(email) INITIALLY DEFERRED WITH (x = 1)": `at "WITH (x = 1)"`,
	}
	for tail, where := range unparsable {
		refusePlan(t, prefix+tail, usersTable(), "cannot parse", where, "plan it by hand")
	}
	refusePlan(t, `ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL NOT VALID`,
		accountsTable(), "cannot parse", `at "NOT VALID"`, "plan it by hand")
	// A comment is not an option: the parse tree reads past it.
	expand, contract := uniqueSQL(t, prefix+"(email) -- trailing comment")
	if expand != `CREATE UNIQUE INDEX CONCURRENTLY "k_idx" ON "public"."users" ("email")` ||
		contract != `ALTER TABLE "public"."users" ADD CONSTRAINT "k" UNIQUE USING INDEX "k_idx"` {
		t.Fatalf("trailing comment changed the plan: %s / %s", expand, contract)
	}
}

func TestPlannerRefusesLongOptionTextTruncated(t *testing.T) {
	parsetree.Require(t)
	long := strings.Repeat("x", 200)
	_, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (email) WITH (` + long + `)`,
		Cycle: 1, Table: usersTable(),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported option") ||
		strings.Contains(err.Error(), long) {
		t.Fatalf("error = %v, want a bounded unsupported-option refusal", err)
	}
}

func TestPlannerRefusesNamesPostgresWouldTruncate(t *testing.T) {
	parsetree.Require(t)
	// "_idx" makes a 59-byte constraint name a 63-byte index name.
	ok := strings.Repeat("c", 59)
	expand, _ := uniqueSQL(t, `ALTER TABLE public.users ADD CONSTRAINT `+ok+` UNIQUE (email)`)
	if !strings.Contains(expand, `"`+ok+`_idx"`) {
		t.Fatalf("63-byte index name not kept: %s", expand)
	}
	tooLong := strings.Repeat("c", 60)
	refusePlan(t, `ALTER TABLE public.users ADD CONSTRAINT `+tooLong+` UNIQUE (email)`,
		usersTable(), tooLong+"_idx", "63 bytes")
	refusePlan(t, `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE ("`+
		strings.Repeat("e", 64)+`")`, usersTable(), "63 bytes")
	refusePlan(t, `ALTER TABLE public.users ADD CONSTRAINT "`+strings.Repeat("é", 32)+
		`" UNIQUE (email)`, usersTable(), "63 bytes")

	// "accounts_" + column + "_nn": a 51-byte column gives a 63-byte proof.
	column := strings.Repeat("e", 51)
	result := mustPlan(t, `ALTER TABLE public.accounts ALTER COLUMN `+column+
		` SET NOT NULL`, accountsTable())
	if !strings.Contains(result.ExpandSteps[0].SQL, `"accounts_`+column+`_nn"`) {
		t.Fatalf("63-byte proof name not kept: %s", result.ExpandSteps[0].SQL)
	}
	refusePlan(t, `ALTER TABLE public.accounts ALTER COLUMN `+column+`e SET NOT NULL`,
		accountsTable(), "accounts_"+column+"e_nn", "63 bytes")
}

func TestPlannerRefusesMalformedColumnLists(t *testing.T) {
	parsetree.Require(t)
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE `
	for _, tail := range []string{
		"()", "( )", "(email", `("email)`, `("")`, "(email email)", "(email.name)",
		"(email, Email)", `(email, "email")`, "(lower(email))", "email",
	} {
		refusePlan(t, prefix+tail, usersTable())
	}
	refusePlan(t, prefix+"()", usersTable(), "cannot parse", "plan it by hand")
	refusePlan(t, prefix+"(email, Email)", usersTable(), `"email"`, "twice")
}

func TestPlannerRefusesUnsupportedStatementShapes(t *testing.T) {
	parsetree.Require(t)
	for _, sql := range []string{
		`ALTER TABLE public.users ADD CONSTRAINT k "UNIQUE" (email)`,
		`ALTER TABLE public.users ADD "CONSTRAINT" k UNIQUE (email)`,
		`ALTER TABLE public.users ADD CONSTRAINT UNIQUE (email)`,
		`ALTER TABLE public.users ADD UNIQUE (email)`,
		`ALTER TABLE public.users ADD CONSTRAINT k PRIMARY KEY (email)`,
		`ALTER TABLE public.users ALTER COLUMN email DROP NOT NULL`,
		`ALTER TABLE public.users ALTER COLUMN email SET NOT`,
		`ALTER TABLE public.users ALTER COLUMN SET NOT NULL`,
		`ALTER TABLE public.users ADD CONSTRAINT Émail_key UNIQUE (email)`,
		`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (Émail)`,
		`ALTER TABLE public.users RENAME COLUMN a TO b`,
	} {
		refusePlan(t, sql, usersTable())
	}
}

func TestPlannerRefusesZeroRequest(t *testing.T) {
	refusePlan(t, "", TableFacts{}, "unsupported migration statement")
	refusePlan(t, "   ;  ", usersTable(), "unsupported migration statement")
}

// TestPlannerIsSafeForConcurrentUse: one Planner serves every MCP
// session, so concurrent plans must not share parse state.
func TestPlannerIsSafeForConcurrentUse(t *testing.T) {
	parsetree.Require(t)
	planner := NewPlanner()
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := `K` + strings.Repeat("x", i)
			sql := `ALTER TABLE public.users ADD CONSTRAINT "` + name +
				`" UNIQUE (Email) DEFERRABLE`
			result, err := planner.Plan(context.Background(),
				Request{SQL: sql, Cycle: i, Table: usersTable()})
			if err != nil || len(result.ExpandSteps) != 1 ||
				!strings.Contains(result.ExpandSteps[0].SQL, `"`+name+`_idx"`) ||
				!strings.HasSuffix(result.ContractSteps[0].SQL, " DEFERRABLE") ||
				result.ContractNotBeforeCycle != i+1 {
				errs <- sql
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for sql := range errs {
		t.Errorf("concurrent plan wrong for %q", sql)
	}
}

func stepSQL(result Plan) []string {
	var out []string
	for _, step := range append(append([]Step(nil), result.ExpandSteps...),
		result.ContractSteps...) {
		out = append(out, step.SQL)
	}
	return out
}

// TestPlannerRefusesTargetOtherThanRequestTable: PostgreSQL reads
// "users . other" as users.other, so a plan written against
// public.users would change a table the SQL never named.
func TestPlannerRefusesTargetOtherThanRequestTable(t *testing.T) {
	parsetree.Require(t)
	for _, sql := range []string{
		`ALTER TABLE users . other ADD CONSTRAINT k UNIQUE (email)`,
		`ALTER TABLE users .other ALTER COLUMN email SET NOT NULL`,
		`ALTER TABLE ONLY users. "users" ADD CONSTRAINT k UNIQUE (email)`,
	} {
		refusePlan(t, sql, usersTable(), "migration SQL targets", "public.users")
	}
	// The spaced form plans when the target it names is the request table.
	same := TableFacts{Schema: "users", Name: "users"}
	got := stepSQL(mustPlan(t, `ALTER TABLE users . users ADD CONSTRAINT k UNIQUE (a)`,
		same))
	want := `CREATE UNIQUE INDEX CONCURRENTLY "k_idx" ON "users"."users" ("a")`
	if len(got) != 2 || got[0] != want {
		t.Fatalf("spaced qualified target: got %q, want first step %q", got, want)
	}
}
