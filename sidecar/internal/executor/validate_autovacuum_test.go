package executor

import "testing"

// TestValidateExecutorSQL_AutovacuumReloptions proves A1's actions pass
// the executor whitelist: ALTER TABLE ... SET/RESET (autovacuum reloptions)
// on a normal schema, and that protected schemas are still rejected.
func TestValidateExecutorSQL_AutovacuumReloptions(t *testing.T) {
	ok := []string{
		`ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.02);`,
		`ALTER TABLE "public"."orders" RESET (autovacuum_vacuum_scale_factor);`,
		`ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)`,
	}
	for _, sql := range ok {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) rejected a valid A1 action: %v", sql, err)
		}
	}
	if err := ValidateExecutorSQL(
		`ALTER TABLE "pg_catalog"."pg_class" SET (autovacuum_vacuum_scale_factor = 0.02)`,
	); err == nil {
		t.Error("ValidateExecutorSQL accepted ALTER on protected schema")
	}
}

// TestValidateExecutorSQL_Analyze confirms A4's ANALYZE actions pass.
func TestValidateExecutorSQL_Analyze(t *testing.T) {
	for _, sql := range []string{`ANALYZE "public"."events";`, `ANALYZE public.orders`} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) rejected ANALYZE: %v", sql, err)
		}
	}
	if err := ValidateExecutorSQL(`ANALYZE "pg_catalog"."pg_class"`); err == nil {
		t.Error("accepted ANALYZE on protected schema")
	}
}

// TestValidateExecutorSQL_VacuumFreeze confirms A6's VACUUM (FREEZE) passes.
func TestValidateExecutorSQL_VacuumFreeze(t *testing.T) {
	if err := ValidateExecutorSQL(`VACUUM (FREEZE) "public"."orders";`); err != nil {
		t.Errorf("rejected VACUUM (FREEZE): %v", err)
	}
	if err := ValidateExecutorSQL(`VACUUM (FREEZE) "pg_catalog"."pg_class"`); err == nil {
		t.Error("accepted VACUUM (FREEZE) on protected schema")
	}
}
