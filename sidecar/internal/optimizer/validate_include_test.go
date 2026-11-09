package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// indexes-11: llm.optimizer.max_include_columns caps the INCLUDE list of a
// proposed index; <= 0 means the default 3. The reason names the cap so
// the tuning agent can re-propose a narrower index.

const (
	include3 = "CREATE INDEX CONCURRENTLY wide_idx ON t (a) INCLUDE (b, c, d)"
	include4 = "CREATE INDEX CONCURRENTLY wide_idx ON t (a) INCLUDE (b, c, d, e)"
)

func includeTable() TableContext {
	var cols []ColumnInfo
	for _, name := range []string{"a", "b", "c", "d", "e", "b,c"} {
		cols = append(cols, ColumnInfo{Name: name, Type: "int"})
	}
	return TableContext{Schema: "public", Table: "t", Columns: cols, ColumnsKnown: true,
		IndexCount: 1}
}

func includeValidator(limit int) *Validator {
	return newTestValidator(&config.OptimizerConfig{MaxIncludeColumns: limit})
}

func TestCheckIncludeWidth_FourAtCapThreeIsRejected(t *testing.T) {
	ok, reason := includeValidator(3).checkIncludeWidth(Recommendation{DDL: include4})
	if ok {
		t.Fatal("an INCLUDE of 4 columns at cap 3 must be rejected")
	}
	for _, want := range []string{"4", "max_include_columns", "3"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q does not name %q", reason, want)
		}
	}
	if !strings.Contains(reason, "at most 3") {
		t.Fatalf("reason %q does not tell the model how to re-propose", reason)
	}
}

func TestCheckIncludeWidth_ExactlyAtCapIsAccepted(t *testing.T) {
	ok, reason := includeValidator(3).checkIncludeWidth(Recommendation{DDL: include3})
	if !ok || reason != "" {
		t.Fatalf("INCLUDE of exactly 3 at cap 3 = %v %q, want accepted", ok, reason)
	}
}

func TestCheckIncludeWidth_ZeroAndNegativeFallBackToThree(t *testing.T) {
	for _, limit := range []int{0, -1, -100} {
		v := includeValidator(limit)
		if ok, reason := v.checkIncludeWidth(Recommendation{DDL: include4}); ok ||
			!strings.Contains(reason, "max_include_columns (3)") {
			t.Fatalf("cap %d: INCLUDE of 4 = %v %q, want rejected at the default 3",
				limit, ok, reason)
		}
		if ok, _ := v.checkIncludeWidth(Recommendation{DDL: include3}); !ok {
			t.Fatalf("cap %d: INCLUDE of 3 rejected; the fallback must be 3, not lower",
				limit)
		}
	}
}

func TestCheckIncludeWidth_ConfiguredCapIsHonoured(t *testing.T) {
	v := includeValidator(2)
	if ok, reason := v.checkIncludeWidth(Recommendation{DDL: include3}); ok ||
		!strings.Contains(reason, "max_include_columns (2)") {
		t.Fatalf("cap 2: INCLUDE of 3 = %v %q, want rejected naming 2", ok, reason)
	}
	two := "CREATE INDEX CONCURRENTLY i ON t (a) INCLUDE (b, c)"
	if ok, _ := v.checkIncludeWidth(Recommendation{DDL: two}); !ok {
		t.Fatal("cap 2: INCLUDE of 2 must be accepted")
	}
	one := includeValidator(1)
	if ok, _ := one.checkIncludeWidth(Recommendation{DDL: two}); ok {
		t.Fatal("cap 1: INCLUDE of 2 must be rejected")
	}
	if ok, _ := includeValidator(5).checkIncludeWidth(Recommendation{DDL: include4}); !ok {
		t.Fatal("cap 5: INCLUDE of 4 must be accepted")
	}
}

func TestCheckIncludeWidth_NoIncludeAndUnparseable(t *testing.T) {
	v := includeValidator(1)
	for _, ddl := range []string{
		"CREATE INDEX CONCURRENTLY i ON t (a, b, c, d, e)", // keys are not INCLUDE
		"CREATE INDEX CONCURRENTLY i ON t (a) INCLUDE (b)",
		"", // unparseable: canonicalization rejects it, not this check
		"DROP TABLE t",
		"CREATE INDEX CONCURRENTLY i ON t (a) INCLUDE (b, c",
	} {
		if ok, reason := v.checkIncludeWidth(Recommendation{DDL: ddl}); !ok {
			t.Fatalf("%q = rejected %q, want accepted by the width check", ddl, reason)
		}
	}
}

// A quoted column name holding a comma is one column, not two.
func TestCheckIncludeWidth_QuotedCommaIsOneColumn(t *testing.T) {
	ddl := `CREATE INDEX CONCURRENTLY i ON t (a) INCLUDE ("b,c", d)`
	if ok, reason := includeValidator(2).checkIncludeWidth(Recommendation{DDL: ddl}); !ok {
		t.Fatalf("INCLUDE (\"b,c\", d) at cap 2 = rejected %q; it has 2 columns", reason)
	}
	ddl = `CREATE INDEX CONCURRENTLY i ON t (a) INCLUDE ("b,c", d, e)`
	if ok, _ := includeValidator(2).checkIncludeWidth(Recommendation{DDL: ddl}); ok {
		t.Fatal("INCLUDE (\"b,c\", d, e) at cap 2 must be rejected: 3 columns")
	}
}

func TestValidate_RunsTheIncludeCap(t *testing.T) {
	v := includeValidator(3)
	ok, reason := v.Validate(context.Background(),
		Recommendation{DDL: include4, IndexType: "btree"}, includeTable())
	if ok || !strings.Contains(reason, "max_include_columns") {
		t.Fatalf("Validate(INCLUDE of 4) = %v %q, want the include cap", ok, reason)
	}
	ok, reason = v.Validate(context.Background(),
		Recommendation{DDL: include3, IndexType: "btree"}, includeTable())
	if !ok {
		t.Fatalf("Validate(INCLUDE of 3) rejected: %q", reason)
	}
}

// A missing INCLUDE column is still reported as missing, not as too wide.
func TestValidate_MissingColumnBeforeIncludeCap(t *testing.T) {
	ddl := "CREATE INDEX CONCURRENTLY i ON t (a) INCLUDE (b, c, d, zz)"
	ok, reason := includeValidator(3).Validate(context.Background(),
		Recommendation{DDL: ddl, IndexType: "btree"}, includeTable())
	if ok || !strings.Contains(reason, "zz does not exist") {
		t.Fatalf("Validate = %v %q, want the missing column", ok, reason)
	}
}

// Through Admit: a too-wide proposal is invalid before any what-if runs,
// and is not remembered as a measured rejection.
func TestAdmit_TooWideIncludeIsInvalidWithoutWhatIf(t *testing.T) {
	store := newMemStore()
	o, w, _ := admissionOptimizer(store, WhatIfResult{Measured: 2, Improvement: 50})
	ddl := "CREATE INDEX CONCURRENTLY ai_claims_wide_idx ON public.ai_claims (status) " +
		"INCLUDE (id, evidence_event_ids_json, x, y)"
	tc := admitTable()
	tc.Columns = append(tc.Columns, ColumnInfo{Name: "x", Type: "int"},
		ColumnInfo{Name: "y", Type: "int"})
	a := o.Admit(context.Background(), candidate(ddl), tc)
	if a.Outcome != AdmitInvalid || !strings.Contains(a.Reason, "max_include_columns (3)") {
		t.Fatalf("admission = %+v, want invalid at the default cap 3", a)
	}
	if w.calls.Load() != 0 || store.records != 0 {
		t.Fatalf("what-ifs %d records %d: the cap must reject before measuring",
			w.calls.Load(), store.records)
	}
}

// No concurrency test: checkIncludeWidth reads an immutable config and the
// DDL; it holds no state.
