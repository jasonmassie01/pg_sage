package executor

import "testing"

// TestValidateExecutorSQL_QuotedIdentifiers proves the Wave-4 quoting
// change (e.g. VACUUM "public"."orders") (a) still passes the executor
// SQL whitelist for normal schemas and (b) does NOT let a quoted
// identifier evade the protected-schema guard.
func TestValidateExecutorSQL_QuotedIdentifiers(t *testing.T) {
	// Valid DDL on a normal schema must pass, quoted or not.
	ok := []string{
		`VACUUM "public"."orders"`,
		`VACUUM "public"."orders";`,
		`REINDEX INDEX CONCURRENTLY "public"."orders_pkey"`,
		`VACUUM public.orders`,
	}
	for _, sql := range ok {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) rejected a valid statement: %v",
				sql, err)
		}
	}

	// A genuinely protected schema (pg_catalog) must STILL be rejected
	// when quoted — quoting cannot be used to evade the guard.
	protected := []string{
		`VACUUM pg_catalog.pg_class`,
		`VACUUM "pg_catalog"."pg_class"`,
		`VACUUM "pg_catalog".pg_class`,
	}
	for _, sql := range protected {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Errorf("ValidateExecutorSQL(%q) accepted DDL on protected schema",
				sql)
		}
	}
}
