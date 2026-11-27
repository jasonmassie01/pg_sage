package auditchain

import (
	"reflect"
	"sort"
	"testing"
)

// No concurrent access tests: Columns and the pinned map are read-only.

// Every built-in spec's current version pins exactly the columns its
// expressions read: a column added to Sealed or State without a pinned
// type fails here, before any database is involved.
func TestSealedColumnTypesCoverEveryBuiltinSpec(t *testing.T) {
	for _, s := range Specs() {
		pinned, ok := SealedColumnTypes[SpecKey{Chain: s.Chain, Version: s.Version}]
		if !ok {
			t.Fatalf("SealedColumnTypes has no entry for %s v%d", s.Chain, s.Version)
		}
		cols := s.Columns()
		if len(cols) != len(s.Sealed)+len(s.State) {
			t.Fatalf("%s: Columns() = %v, want one per Sealed and State expression",
				s.Chain, cols)
		}
		var keys []string
		for k, typ := range pinned {
			if typ == "" {
				t.Fatalf("%s.%s pins an empty type", s.Chain, k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sorted := append([]string(nil), cols...)
		sort.Strings(sorted)
		if !reflect.DeepEqual(keys, sorted) {
			t.Fatalf("%s v%d pins %v, its expressions read %v", s.Chain, s.Version, keys,
				sorted)
		}
	}
}

// Columns names each column once, in expression order, through casts and
// the epoch wrapper.
func TestSpecColumns(t *testing.T) {
	s := Spec{Sealed: []string{col("id"), epochUS("at"), col("db") + "::text"},
		State: []string{col("outcome"), col("id")}}
	if got, want := s.Columns(), []string{"id", "at", "db", "outcome"}; !reflect.DeepEqual(
		got, want) {
		t.Fatalf("Columns() = %v, want %v", got, want)
	}
	if got := (Spec{}).Columns(); len(got) != 0 {
		t.Fatalf("empty spec Columns() = %v, want none", got)
	}
	if got := (Spec{Sealed: []string{"now()"}}).Columns(); len(got) != 0 {
		t.Fatalf("an expression naming no column gave %v", got)
	}
}

func TestSpecColumnsOfActionLog(t *testing.T) {
	cols := ActionLog.Columns()
	if cols[0] != "id" || cols[1] != "executed_at" || cols[len(cols)-1] != "toil_model_version" {
		t.Fatalf("ActionLog.Columns() = %v", cols)
	}
}
