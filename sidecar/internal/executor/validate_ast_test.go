//go:build cgo

package executor

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
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

// ALTER ROLE ... CONNECTION LIMIT reaches the parse tree with the
// catalog-free role budget rule: postgres, pg_ and provider administration
// roles are refused before run time (env-cgo).
func TestCheckParseTreeRoleConnectionLimit(t *testing.T) {
	for _, sql := range []string{
		"ALTER ROLE postgres CONNECTION LIMIT 5",
		"ALTER ROLE pg_monitor CONNECTION LIMIT 5",
		"ALTER ROLE cloudsqladmin CONNECTION LIMIT 5",
		"ALTER ROLE rdsadmin CONNECTION LIMIT -1",
		`ALTER ROLE U&"\0070ostgres" CONNECTION LIMIT 5`,
		"ALTER ROLE budget_exec_app CONNECTION LIMIT 5 SUPERUSER",
	} {
		err := checkParseTree(sql)
		if !errors.Is(err, ErrDisallowedSQL) || !errors.Is(err, sqlast.ErrRejected) {
			t.Errorf("checkParseTree(%q) = %v, want ErrDisallowedSQL wrapping "+
				"sqlast.ErrRejected", sql, err)
		}
	}
	for _, sql := range []string{
		`ALTER ROLE "budget_exec_app" CONNECTION LIMIT 10`,
		"ALTER ROLE app CONNECTION LIMIT 0",
		"ALTER ROLE app IN DATABASE db SET statement_timeout = '30s'",
	} {
		if err := checkParseTree(sql); err != nil {
			t.Errorf("checkParseTree(%q) = %v, want nil", sql, err)
		}
	}
}
