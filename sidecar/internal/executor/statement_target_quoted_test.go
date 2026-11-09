package executor

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// No concurrent access tests: the statement target and the binding are
// pure functions of their text (TestFindingRequestBindingConcurrent covers
// concurrent callers of the binding).

// executor-08 review: the analyzer rules and the freeze custodian name a
// relation in catalog form ("public.my table") and quote it in their SQL.
// A name holding a space or a dot is the same relation, so binding must
// not park the change as target_mismatch.
func TestCatalogNamesWithSpacesAndDotsBind(t *testing.T) {
	exec := bindingExecutor()
	cases := []struct{ ident, sql string }{
		{"public.my table", `VACUUM (FREEZE) "public"."my table"`},
		{"public.a.b", `VACUUM (FREEZE) "public"."a.b"`},
		{"Sales Ops.Events", `ANALYZE "Sales Ops"."Events"`},
		{"public.my table", `ALTER TABLE "public"."my table" SET ` +
			`(autovacuum_vacuum_scale_factor = 0.05)`},
	}
	for _, tc := range cases {
		f := analyzer.Finding{Category: "table_bloat", ObjectType: "table",
			ObjectIdentifier: tc.ident, RecommendedSQL: tc.sql, ActionRisk: "safe"}
		if _, err := BindFindingTarget(f); err != nil {
			t.Errorf("BindFindingTarget(%q, %q) = %v, want the same relation",
				tc.ident, tc.sql, err)
		}
		d := exec.evaluateFindingPolicy(context.Background(), f, false)
		if d.BlockedReason == "target_mismatch" {
			t.Errorf("%q on %q refused as target_mismatch: %s", tc.sql, tc.ident, d.Detail)
		}
	}
	p := CustodianProposal{Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."my table"`,
		TargetObjects: []string{"public.my table"}}
	if d := exec.EvaluateCustodianProposal(context.Background(), p); d.BlockedReason ==
		"target_mismatch" {
		t.Fatalf("freeze custodian on a spaced table refused: %s", d.Detail)
	}
}

// The catalog form still binds one relation: another table, another
// schema or a name that only shares a prefix is a mismatch.
func TestCatalogNamesStillRefuseAnotherRelation(t *testing.T) {
	cases := []struct{ ident, sql string }{
		{"public.my table", `VACUUM (FREEZE) "public"."my tables"`},
		{"public.my table", `VACUUM (FREEZE) "other"."my table"`},
		{"public.a.b", `VACUUM (FREEZE) "public"."a"`},
		{"public.my table", `VACUUM (FREEZE) "public"."my"`},
	}
	for _, tc := range cases {
		f := analyzer.Finding{ObjectType: "table", ObjectIdentifier: tc.ident,
			RecommendedSQL: tc.sql}
		var mismatch *TargetMismatchError
		if _, err := BindFindingTarget(f); !errors.As(err, &mismatch) {
			t.Errorf("BindFindingTarget(%q, %q) = %v, want target_mismatch",
				tc.ident, tc.sql, err)
		}
	}
}

// A quoted identifier holding a space is one statement target, for the
// operator lease as for the binding.
func TestStatementTargetKeepsQuotedSpaces(t *testing.T) {
	cases := map[string]string{
		`VACUUM (FREEZE, VERBOSE) "public"."my table"`:            `"public"."my table"`,
		`ANALYZE VERBOSE "my schema"."t"`:                         `"my schema"."t"`,
		`ALTER TABLE ONLY "public"."a b" SET (fillfactor = 90)`:   `"public"."a b"`,
		`CREATE INDEX CONCURRENTLY "idx x" ON "public"."a b" (c)`: `"public"."a b"`,
		`DROP INDEX CONCURRENTLY IF EXISTS "public"."idx x"`:      `"public"."idx x"`,
		`REINDEX INDEX CONCURRENTLY "public"."idx x"`:             `"public"."idx x"`,
		"VACUUM public.t": "public.t",
	}
	for sql, want := range cases {
		got := operatorLeaseTargets(sql)
		if !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("operatorLeaseTargets(%q) = %q, want [%q]", sql, got, want)
		}
	}
}

// An index name may hold " ON public.t ": the protected-schema check must
// read the table after the real ON, not one inside the quoted name.
func TestProtectedSchemaCheckReadsPastQuotedIndexName(t *testing.T) {
	sql := `CREATE INDEX CONCURRENTLY " ON public.t " ON sage.action_log (id)`
	if err := ValidateExecutorSQL(sql); !errors.Is(err, ErrDisallowedSQL) {
		t.Fatalf("ValidateExecutorSQL(%q) = %v, want the protected-schema refusal", sql, err)
	}
	control := `CREATE INDEX CONCURRENTLY " ON sage.t " ON public.orders (id)`
	if got := operatorLeaseTargets(control); !reflect.DeepEqual(got,
		[]string{"public.orders"}) {
		t.Fatalf("operatorLeaseTargets(%q) = %q, want [public.orders]", control, got)
	}
}
