package plan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// No concurrent access tests: the planner holds only its delay, set
// before use, and ContractScript is a pure function of the plan.

const contractNotNullSQL = `ALTER TABLE public.users ALTER COLUMN email SET NOT NULL`
const contractUniqueSQL = `ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`

func planWith(t *testing.T, planner *Planner, request Request) Plan {
	t.Helper()
	planned, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan(%q): %v", request.SQL, err)
	}
	return planned
}

// State transition: the contract waits a wall-clock delay (default 1h),
// not a count of cycles.
func TestPlannerContractDelay(t *testing.T) {
	parsetree.Require(t)
	request := Request{SQL: contractNotNullSQL, Cycle: 3, Table: usersTable()}
	if got := planWith(t, NewPlanner(), request).ContractNotBefore; got != time.Hour {
		t.Fatalf("default ContractNotBefore = %s, want 1h", got)
	}
	planner := NewPlanner().WithContractDelay(90 * time.Minute)
	if got := planWith(t, planner, request).ContractNotBefore; got != 90*time.Minute {
		t.Fatalf("configured ContractNotBefore = %s, want 1h30m", got)
	}
	for _, bad := range []time.Duration{0, -time.Minute} {
		planner := NewPlanner().WithContractDelay(bad)
		if got := planWith(t, planner, request).ContractNotBefore; got != time.Hour {
			t.Fatalf("delay %s: ContractNotBefore = %s, want the 1h default", bad, got)
		}
	}
	var nilPlanner *Planner
	if got := nilPlanner.WithContractDelay(time.Minute); got != nil {
		t.Fatalf("nil planner WithContractDelay = %#v", got)
	}
}

// Every planned step states the lock it takes.
func TestPlannerDeclaresStepLocks(t *testing.T) {
	parsetree.Require(t)
	want := map[StepKind]string{
		StepAddCheckNotValid: "ACCESS EXCLUSIVE", StepValidateConstraint: "SHARE UPDATE EXCLUSIVE",
		StepSetNotNull: "ACCESS EXCLUSIVE", StepDropProofConstraint: "ACCESS EXCLUSIVE",
		StepCreateUniqueIndex:      "SHARE UPDATE EXCLUSIVE",
		StepAttachUniqueConstraint: "ACCESS EXCLUSIVE",
	}
	for _, sql := range []string{contractNotNullSQL, contractUniqueSQL} {
		planned := planWith(t, NewPlanner(), Request{SQL: sql, Table: usersTable()})
		for _, step := range append(planned.ExpandSteps, planned.ContractSteps...) {
			if step.LockLevel != want[step.Kind] {
				t.Fatalf("%s lock = %q, want %q", step.Kind, step.LockLevel, want[step.Kind])
			}
		}
	}
}

// StepLock never reports weaker than the plan states, and an unstated
// lock is ACCESS EXCLUSIVE (fail closed).
func TestStepLock(t *testing.T) {
	ctx := context.Background()
	declared := Step{SQL: `ALTER TABLE "public"."users" VALIDATE CONSTRAINT "users_email_nn"`,
		LockLevel: "SHARE UPDATE EXCLUSIVE"}
	if got := StepLock(ctx, declared); lockRank[got] < lockRank["SHARE UPDATE EXCLUSIVE"] {
		t.Fatalf("StepLock(validate) = %q", got)
	}
	unstated := Step{SQL: `ALTER TABLE "public"."users" VALIDATE CONSTRAINT "x"`}
	if got := StepLock(ctx, unstated); got != lockAccessExclusive {
		t.Fatalf("StepLock(unstated) = %q, want ACCESS EXCLUSIVE", got)
	}
	setNotNull := Step{SQL: `ALTER TABLE "public"."users" ALTER COLUMN "email" SET NOT NULL`,
		LockLevel: "SHARE"}
	if got := StepLock(ctx, setNotNull); lockRank[got] < lockRank["SHARE"] {
		t.Fatalf("StepLock(set not null) = %q, weaker than stated", got)
	}
}

// Happy path: the SET NOT NULL forward-fix script asserts the proof
// constraint is validated, bounds lock waits, then runs both contract
// steps in order inside one transaction.
func TestContractScriptSetNotNull(t *testing.T) {
	parsetree.Require(t)
	planned := planWith(t, NewPlanner(), Request{SQL: contractNotNullSQL, Table: usersTable()})
	script, err := planned.ContractScript()
	if err != nil {
		t.Fatalf("ContractScript: %v", err)
	}
	header := script[:strings.Index(script, "SET LOCAL lock_timeout")]
	for _, want := range []string{"users_email_nn", "convalidated", "RAISE EXCEPTION"} {
		if !strings.Contains(header, want) {
			t.Fatalf("header lacks %q:\n%s", want, script)
		}
	}
	assertInOrder(t, script, "BEGIN;", "DO $pg_sage_guard$", "SET LOCAL lock_timeout = '5000ms';",
		planned.ContractSteps[0].SQL+";", planned.ContractSteps[1].SQL+";", "COMMIT;")
	if planned.ContractGuard.Kind != GuardValidatedCheck ||
		planned.ContractGuard.Name != "users_email_nn" {
		t.Fatalf("ContractGuard = %#v", planned.ContractGuard)
	}
}

func TestContractScriptUniqueChecksIndexValid(t *testing.T) {
	parsetree.Require(t)
	planned := planWith(t, NewPlanner(), Request{SQL: contractUniqueSQL, Table: usersTable()})
	script, err := planned.ContractScript()
	if err != nil {
		t.Fatalf("ContractScript: %v", err)
	}
	for _, want := range []string{"indisvalid", "indisunique", `'"public"."users_email_key_idx"'`,
		"UNIQUE USING INDEX"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
}

// An operator-supplied proof is what the script asserts.
func TestContractScriptUsesSuppliedProof(t *testing.T) {
	parsetree.Require(t)
	planned := planWith(t, NewPlanner(), Request{SQL: contractNotNullSQL, Table: usersTable(),
		Proofs: []Proof{{Kind: ProofValidatedNotNullCheck, Column: "email",
			Constraint: "email_present", Validated: true}}})
	script, err := planned.ContractScript()
	if err != nil {
		t.Fatalf("ContractScript: %v", err)
	}
	if !strings.Contains(script, "'email_present'") || len(planned.ContractSteps) != 1 {
		t.Fatalf("script = %s\nsteps = %#v", script, planned.ContractSteps)
	}
}

// Invalid input: literals are quoted, and a plan the header cannot
// guard is refused instead of rendered unguarded.
func TestContractScriptRefusesUnguardablePlans(t *testing.T) {
	parsetree.Require(t)
	quoted := planWith(t, NewPlanner(), Request{
		SQL:   `ALTER TABLE public."o'brien" ALTER COLUMN email SET NOT NULL`,
		Table: TableFacts{Schema: "public", Name: "o'brien"}})
	script, err := quoted.ContractScript()
	if err != nil || !strings.Contains(script, `'o''brien_email_nn'`) {
		t.Fatalf("quoted script err=%v:\n%s", err, script)
	}
	tests := map[string]func(*Plan){
		"no contract steps": func(p *Plan) { p.ContractSteps = nil },
		"no guard name":     func(p *Plan) { p.ContractGuard.Name = "" },
		"unknown guard":     func(p *Plan) { p.ContractGuard.Kind = "" },
		"dollar tag in name": func(p *Plan) {
			p.ContractGuard.Name = "x$pg_sage_guard$y"
		},
		"top-level contract step": func(p *Plan) { p.ContractSteps[0].RequiresTopLevel = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			planned := planWith(t, NewPlanner(),
				Request{SQL: contractNotNullSQL, Table: usersTable()})
			mutate(&planned)
			if script, err := planned.ContractScript(); err == nil {
				t.Fatalf("ContractScript accepted it:\n%s", script)
			}
		})
	}
}

func assertInOrder(t *testing.T, text string, parts ...string) {
	t.Helper()
	at := 0
	for _, part := range parts {
		index := strings.Index(text[at:], part)
		if index < 0 {
			t.Fatalf("%q missing or out of order in:\n%s", part, text)
		}
		at += index + len(part)
	}
}
