//go:build integration

package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// Integration (migration-06 review): the forward-fix script's safety
// claims hold on PostgreSQL the way an operator runs it.

// scriptStatements splits the script the way psql sends it: one
// statement per ";" line end, with the dollar-quoted guard kept whole.
func scriptStatements(script string) []string {
	var statements []string
	var current strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		current.WriteString(line)
		text := current.String()
		if strings.HasSuffix(strings.TrimSpace(line), ";") &&
			strings.Count(text, guardTag)%2 == 0 {
			statements = append(statements, text)
			current.Reset()
		}
	}
	return statements
}

func proofConstraintExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1
		FROM pg_constraint WHERE conname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("read constraint %s: %v", name, err)
	}
	return exists
}

// A client that keeps going after an error (psql without ON_ERROR_STOP)
// sends every statement; the raised guard still leaves the table as it
// was, because the later statements run in the aborted transaction.
func TestContractScriptIgnoringErrorsChangesNothing(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetContractTable(t, pool)
	planned, script := renderScript(t,
		`ALTER TABLE public.mig06_users ALTER COLUMN email SET NOT NULL`)
	execExpandSteps(t, pool, planned.ExpandSteps[:1]) // NOT VALID only
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	statements := scriptStatements(script)
	if len(statements) != 6 {
		t.Fatalf("split %d statements, want 6:\n%s", len(statements), script)
	}
	var guardErr error
	for i, statement := range statements {
		_, err := conn.Exec(context.Background(), statement)
		if i == 1 {
			guardErr = err
		}
	}
	if guardErr == nil || !strings.Contains(guardErr.Error(), "mig06_users_email_nn") {
		t.Fatalf("guard error = %v, want one naming the proof", guardErr)
	}
	if !columnNullable(t, pool) || !proofConstraintExists(t, pool, "mig06_users_email_nn") {
		t.Fatal("a statement after the raised guard changed the table")
	}
	if status := conn.Conn().PgConn().TxStatus(); status != 'I' {
		t.Fatalf("transaction status %q after COMMIT, want idle", status)
	}
}

// A proof name with a backslash guards correctly when the operator's
// session has standard_conforming_strings off.
func TestContractScriptBackslashNameWithoutStandardStrings(t *testing.T) {
	parsetree.Require(t)
	pool := planDBPool(t)
	resetContractTable(t, pool)
	const proof = `mig06_chk\x`
	if _, err := pool.Exec(context.Background(), `ALTER TABLE public.mig06_users
		ADD CONSTRAINT "mig06_chk\x" CHECK (email IS NOT NULL)`); err != nil {
		t.Fatalf("add proof: %v", err)
	}
	planned, err := NewPlanner().Plan(context.Background(), Request{
		SQL:   `ALTER TABLE public.mig06_users ALTER COLUMN email SET NOT NULL`,
		Table: contractTable(), Proofs: []Proof{{Kind: ProofValidatedNotNullCheck,
			Column: "email", Constraint: proof, Validated: true}}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	script, err := planned.ContractScript()
	if err != nil {
		t.Fatalf("ContractScript: %v", err)
	}
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(),
		`SET standard_conforming_strings = off; SET escape_string_warning = off`); err != nil {
		t.Fatalf("set standard_conforming_strings: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `RESET standard_conforming_strings`)
	}()
	if _, err := conn.Exec(context.Background(), script); err != nil {
		t.Fatalf("script with a validated backslash proof: %v\n%s", err, script)
	}
	if columnNullable(t, pool) {
		t.Fatal("the column is still nullable after the script ran")
	}
}
