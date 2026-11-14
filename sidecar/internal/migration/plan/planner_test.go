package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

func TestPlannerRewritesAddUniqueWithoutBlockingIndexBuild(t *testing.T) {
	parsetree.Require(t)
	planner := NewPlanner()
	result, err := planner.Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`,
		Cycle: 17,
		Table: TableFacts{Schema: "public", Name: "users", EstimatedRows: 80_000_000},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Classification != ClassificationAddUnique || !result.Rewritten {
		t.Fatalf("classification/rewrite = %#v", result)
	}
	if len(result.ExpandSteps) != 1 || len(result.ContractSteps) != 1 {
		t.Fatalf("steps = expand %#v contract %#v",
			result.ExpandSteps, result.ContractSteps)
	}
	indexStep := result.ExpandSteps[0]
	if indexStep.Kind != StepCreateUniqueIndex || !indexStep.RequiresTopLevel ||
		!strings.Contains(indexStep.SQL, "CREATE UNIQUE INDEX CONCURRENTLY") ||
		!strings.Contains(indexStep.SQL, `"public"."users"`) ||
		!strings.Contains(indexStep.SQL, `("email")`) {
		t.Fatalf("index step = %#v", indexStep)
	}
	constraintStep := result.ContractSteps[0]
	if constraintStep.Kind != StepAttachUniqueConstraint ||
		!strings.Contains(constraintStep.SQL, "UNIQUE USING INDEX") {
		t.Fatalf("constraint step = %#v", constraintStep)
	}
	assertNoUnsafeOriginal(t, result,
		"ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)")
}

func TestPlannerRewritesHugeSetNotNullWithoutValidatedProof(t *testing.T) {
	parsetree.Require(t)
	result, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL`,
		Cycle: 41,
		Table: TableFacts{
			Schema: "public", Name: "accounts", EstimatedRows: 500_000_000,
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Classification != ClassificationSetNotNull || !result.Rewritten ||
		!result.RequiresRehearsal {
		t.Fatalf("plan = %#v", result)
	}
	if len(result.ExpandSteps) != 2 || len(result.ContractSteps) != 2 {
		t.Fatalf("steps = expand %#v contract %#v",
			result.ExpandSteps, result.ContractSteps)
	}
	assertStep(t, result.ExpandSteps[0], StepAddCheckNotValid,
		"CHECK (\"email\" IS NOT NULL) NOT VALID")
	assertStep(t, result.ExpandSteps[1], StepValidateConstraint,
		"VALIDATE CONSTRAINT")
	assertStep(t, result.ContractSteps[0], StepSetNotNull,
		"ALTER COLUMN \"email\" SET NOT NULL")
	assertStep(t, result.ContractSteps[1], StepDropProofConstraint,
		"DROP CONSTRAINT")
	if result.ContractNotBeforeCycle != 42 {
		t.Fatalf("ContractNotBeforeCycle = %d, want 42",
			result.ContractNotBeforeCycle)
	}
}

func TestPlannerUsesValidatedNotNullProofButStillDelaysContract(t *testing.T) {
	parsetree.Require(t)
	result, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL`,
		Cycle: 9,
		Table: TableFacts{
			Schema: "public", Name: "accounts", EstimatedRows: 500_000_000,
		},
		Proofs: []Proof{{
			Kind: ProofValidatedNotNullCheck, Column: "email",
			Constraint: "accounts_email_nn", Validated: true,
		}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Rewritten || len(result.ExpandSteps) != 0 {
		t.Fatalf("validated proof caused redundant rewrite: %#v", result)
	}
	if len(result.ContractSteps) != 1 ||
		result.ContractSteps[0].Kind != StepSetNotNull {
		t.Fatalf("contract steps = %#v", result.ContractSteps)
	}
	if result.ContractNotBeforeCycle != 10 {
		t.Fatalf("ContractNotBeforeCycle = %d, want 10",
			result.ContractNotBeforeCycle)
	}
}

func TestPlanNeverReturnsContractInExpandCycle(t *testing.T) {
	parsetree.Require(t)
	result, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL`,
		Cycle: 100,
		Table: TableFacts{
			Schema: "public", Name: "accounts", EstimatedRows: 500_000_000,
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	current := result.StepsForCycle(100, PhaseExpandComplete)
	for _, step := range current {
		if step.Phase == PhaseContract || step.Destructive {
			t.Fatalf("expand cycle exposed contract step %#v", step)
		}
	}
	beforeBarrier := result.StepsForCycle(100, PhaseExpandVerified)
	if len(beforeBarrier) != 0 {
		t.Fatalf("same-cycle verified expand exposed contract: %#v", beforeBarrier)
	}
	next := result.StepsForCycle(101, PhaseExpandVerified)
	if len(next) == 0 || next[0].Phase != PhaseContract {
		t.Fatalf("later cycle contract steps = %#v", next)
	}
}

func TestPlannerPropagatesCancellationWithoutPlan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := NewPlanner().Plan(ctx, Request{
		SQL:   `ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`,
		Cycle: 1,
		Table: TableFacts{Schema: "public", Name: "users", EstimatedRows: 1},
	})

	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Plan error = %v", err)
	}
	if len(result.ExpandSteps) != 0 || len(result.ContractSteps) != 0 {
		t.Fatalf("cancelled planner returned steps %#v", result)
	}
}

func assertStep(t *testing.T, step Step, kind StepKind, sqlFragment string) {
	t.Helper()
	if step.Kind != kind || !strings.Contains(step.SQL, sqlFragment) {
		t.Fatalf("step = %#v, want kind %q containing %q", step, kind, sqlFragment)
	}
}

func assertNoUnsafeOriginal(t *testing.T, result Plan, original string) {
	t.Helper()
	for _, step := range append(
		append([]Step(nil), result.ExpandSteps...), result.ContractSteps...,
	) {
		if strings.EqualFold(strings.TrimSpace(step.SQL), strings.TrimSpace(original)) {
			t.Fatalf("unsafe original DDL survived rewrite: %#v", step)
		}
	}
}

// migration-02b: the planner reads the statement from the parse tree, so
// a comment means what PostgreSQL says it means, never a clause.
func TestPlannerReadsCommentsAsTheParseTreeDoes(t *testing.T) {
	parsetree.Require(t)
	got := stepSQL(mustPlan(t, `ALTER TABLE public.users ADD CONSTRAINT k `+
		`UNIQUE /* ) INCLUDE ( */ (a) /* DEFERRABLE */`, usersTable()))
	want := []string{
		`CREATE UNIQUE INDEX CONCURRENTLY "k_idx" ON "public"."users" ("a")`,
		`ALTER TABLE "public"."users" ADD CONSTRAINT "k" UNIQUE USING INDEX "k_idx"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan\n got %q\nwant %q", got, want)
	}
}

// The planner keeps the parse-tree refusal in its error chain, so callers
// can tell a statement shape from an option, a syntax error or a name.
func TestPlannerKeepsTheNamedParseTreeRefusal(t *testing.T) {
	parsetree.Require(t)
	long := strings.Repeat("c", 64)
	cases := []struct {
		sql  string
		want error
	}{
		{`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (a) INCLUDE (b)`,
			sqlast.ErrUnsupportedOption},
		{`ALTER TABLE public.users ADD CONSTRAINT k PRIMARY KEY (a)`, sqlast.ErrNotPlannable},
		{`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE ()`, sqlast.ErrSyntax},
		{`ALTER TABLE public.users ALTER COLUMN ` + long + ` SET NOT NULL`,
			sqlast.ErrNameTooLong},
	}
	for _, tc := range cases {
		result, err := NewPlanner().Plan(context.Background(),
			Request{SQL: tc.sql, Cycle: 1, Table: usersTable()})
		if !errors.Is(err, tc.want) {
			t.Fatalf("Plan(%q) error = %v, want %v", tc.sql, err, tc.want)
		}
		if len(result.ExpandSteps) != 0 || len(result.ContractSteps) != 0 {
			t.Fatalf("Plan(%q) refused but returned %#v", tc.sql, result)
		}
	}
}
