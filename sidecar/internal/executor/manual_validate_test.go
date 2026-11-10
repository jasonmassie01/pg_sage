package executor

import (
	"errors"
	"testing"
)

// ExecuteManual validates both the SQL and its rollback with
// ValidateExecutorSQL before touching the database (manual.go). The forms an
// operator approves by hand must pass it; this replaces a test that
// recovered a nil-pool panic and asserted nothing (ci-11). That test's
// rollback, "DROP INDEX idx", is in fact refused (unqualified).
func TestValidateExecutorSQL_AcceptsManualForms(t *testing.T) {
	for _, sql := range []string{
		// security-01: a CREATE INDEX target must be schema-qualified.
		"CREATE INDEX idx ON public.t (c)",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_customer " +
			"ON public.orders (customer_id)",
		"CREATE UNIQUE INDEX CONCURRENTLY idx_u ON public.orders (id)",
		"DROP INDEX public.idx",
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_customer",
		"REINDEX INDEX CONCURRENTLY public.idx_orders_customer",
		"VACUUM (ANALYZE) public.orders",
		"ANALYZE public.orders",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)",
		"ALTER SYSTEM SET work_mem = '64MB'",
		"CREATE STATISTICS public.sage_stx_orders ON customer_id, region FROM public.orders",
		"DROP STATISTICS IF EXISTS public.sage_stx_orders",
		"  CREATE INDEX idx ON public.t (c);  ",
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want nil", sql, err)
		}
	}
}

func TestValidateExecutorSQL_RejectsNonManualForms(t *testing.T) {
	for _, sql := range []string{
		"",
		"   ",
		"DROP TABLE t",
		"CREATE INDEX idx ON t (c); DROP TABLE t",
		"CREATE INDEX idx ON t (c) -- note",
		"TRUNCATE public.orders",
		// The rollback the deleted test used: unqualified drops are refused.
		"DROP INDEX idx",
		"DROP STATISTICS IF EXISTS public.s_orders",
	} {
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL", sql, err)
		}
	}
}
