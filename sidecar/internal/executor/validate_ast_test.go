//go:build cgo

package executor

import (
	"errors"
	"testing"
)

// Text validation accepts these, only the parse tree sees the problem.
func TestValidateExecutorSQLParseTreeLayer(t *testing.T) {
	if !ASTValidationAvailable() {
		t.Fatal("cgo build without AST validation")
	}
	for _, sql := range []string{
		`ANALYZE U&"\0073age".findings`,
		"ALTER TABLE public.orders SET (fillfactor = 70), SET LOGGED",
		"SELECT pg_cancel_backend(4711) UNION SELECT pg_terminate_backend(1)",
	} {
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL", sql, err)
		}
	}
}

func TestValidateExecutorSQLParseTreeAcceptsExecutorSQL(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY idx_a ON public.orders (status)",
		"ALTER SYSTEM SET work_mem = '64MB'",
		"SELECT pg_terminate_backend(42)",
		"DELETE FROM hint_plan.hints WHERE query_id = 1",
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want nil", sql, err)
		}
	}
}
