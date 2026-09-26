package executor

import (
	"strings"
	"testing"
)

// Regression tests for G4-B07 (whitelist bypass via ALTER TABLE lists and
// whitespace/comments), G4-B21 (session-safety GUCs allowlisted), G4-B22
// (SET/RESET allowlisted) and G4-D20 (pg_reload_conf allowlisted).

func TestValidateRejectsAlterTableSubcommandLists(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor=0.01), DROP COLUMN email",
		"ALTER TABLE public.orders SET (autovacuum_enabled = on) , ALTER COLUMN id TYPE text",
		"ALTER TABLE public.orders RESET (autovacuum_enabled), DROP CONSTRAINT c",
		"ALTER TABLE public.orders SET (fillfactor = 70) SET TABLESPACE fast",
		"ALTER TABLE public.orders SET TABLESPACE fast",
		"ALTER TABLE public.orders SET (autovacuum_enabled = on) /* x */, DROP COLUMN a",
	} {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Fatalf("ValidateExecutorSQL(%q) accepted a non-reloption subcommand", sql)
		}
		if got := actionTypeForProposalSQL(sql); got == "set_table_autovacuum" {
			t.Fatalf("%q classified as set_table_autovacuum", sql)
		}
	}
}

func TestValidateAcceptsSingleReloptionSubcommand(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)",
		"ALTER TABLE \"public\".\"Orders\" SET (autovacuum_vacuum_scale_factor = 0.02, " +
			"autovacuum_analyze_scale_factor = 0.01);",
		"ALTER TABLE public.orders RESET (autovacuum_vacuum_scale_factor);",
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Fatalf("ValidateExecutorSQL(%q) = %v, want nil", sql, err)
		}
	}
}

func TestVacuumFullIsNeverClassifiedAsSafeVacuum(t *testing.T) {
	for _, sql := range []string{
		"VACUUM  FULL public.orders",
		"VACUUM\tFULL public.orders",
		"vacuum\nfull public.orders",
		"VACUUM /**/ FULL public.orders",
		"VACUUM (VERBOSE,  FULL) public.orders",
	} {
		if got := actionTypeForProposalSQL(sql); got == "vacuum_table" {
			t.Fatalf("%q classified as safe vacuum_table", sql)
		}
	}
	if got := actionTypeForProposalSQL("VACUUM  public.orders"); got != "vacuum_table" {
		t.Fatalf("plain VACUUM with double space = %q, want vacuum_table", got)
	}
}

func TestValidateRejectsSQLComments(t *testing.T) {
	for _, sql := range []string{
		"VACUUM /**/ FULL public.orders",
		"CREATE INDEX CONCURRENTLY idx ON public.t (a) -- trailing",
		"ANALYZE /* hidden */ public.t",
	} {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Fatalf("ValidateExecutorSQL(%q) accepted SQL comments", sql)
		}
	}
	hint := "INSERT INTO hint_plan.hints (norm_query_string, application_name, hints) " +
		"VALUES ('/* app */ SELECT 1 -- x', '', 'SeqScan(t)')"
	if err := ValidateExecutorSQL(hint); err != nil {
		t.Fatalf("comment markers inside string literals rejected: %v", err)
	}
}

func TestValidateRejectsSessionSafetyGUCs(t *testing.T) {
	for _, param := range []string{
		"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
	} {
		for _, sql := range []string{
			"ALTER SYSTEM SET " + param + " = 100",
			"ALTER DATABASE app SET " + param + " = 100",
		} {
			err := ValidateExecutorSQL(sql)
			if err == nil || !strings.Contains(err.Error(), "disallowed") {
				t.Fatalf("ValidateExecutorSQL(%q) = %v, want disallowed", sql, err)
			}
		}
	}
}

func TestValidateRejectsSessionStateStatements(t *testing.T) {
	for _, sql := range []string{
		"SET search_path = public, pg_catalog",
		"SET ROLE postgres",
		"SET session_replication_role = replica",
		"RESET ALL",
		"RESET search_path",
		"SELECT pg_reload_conf()",
	} {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Fatalf("ValidateExecutorSQL(%q) accepted session/config control SQL", sql)
		}
	}
}
