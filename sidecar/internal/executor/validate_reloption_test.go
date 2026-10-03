package executor

import (
	"errors"
	"testing"
)

// The executor's own reloption allowlist (G-P0-1): whatever produced the
// SQL (advisor, analyzer rule, custodian, an operator approval), the
// executor never disables autovacuum and never sets an unknown storage
// parameter.
func TestValidateExecutorSQL_ReloptionAllowlist(t *testing.T) {
	refused := []string{
		`ALTER TABLE public.orders SET (autovacuum_enabled = false)`,
		`ALTER TABLE public.orders SET (autovacuum_enabled = off);`,
		`ALTER TABLE public.orders SET (autovacuum_enabled = 0)`,
		`ALTER TABLE public.orders SET (autovacuum_enabled = 'no')`,
		`ALTER TABLE public.orders SET ("autovacuum_enabled" = false)`,
		`ALTER TABLE public.orders SET (toast.autovacuum_enabled = false)`,
		`ALTER TABLE public.orders SET (fillfactor = 90, autovacuum_enabled = false)`,
		`ALTER TABLE public.orders SET (parallel_workers = 8)`,
		`ALTER TABLE public.orders SET (user_catalog_table = true)`,
		`ALTER TABLE public.orders SET (vacuum_truncate = false)`,
		`ALTER TABLE public.orders RESET (parallel_workers)`,
	}
	for _, sql := range refused {
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL", sql, err)
		}
	}
	allowed := []string{
		`ALTER TABLE public.orders SET (autovacuum_enabled = true)`,
		`ALTER TABLE public.orders SET (autovacuum_enabled = on)`,
		`ALTER TABLE public.orders RESET (autovacuum_enabled)`,
		`ALTER TABLE public.orders SET (fillfactor = 90)`,
		`ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.02, ` +
			`autovacuum_vacuum_threshold = 1000)`,
		`ALTER TABLE public.orders SET (toast.autovacuum_vacuum_scale_factor = 0.05)`,
		`ALTER TABLE public.orders RESET (fillfactor, autovacuum_vacuum_threshold)`,
	}
	for _, sql := range allowed {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want nil", sql, err)
		}
	}
}
