package workpack

import (
	"reflect"
	"testing"
)

// The leak guard's scanners. No DB or concurrency test here: they are pure
// functions of a string.

func TestStringLiterals_EveryQuotingForm(t *testing.T) {
	for in, want := range map[string][]string{
		`a = 'x' AND b = 'it''s'`:             {"x", "it's"},
		`E'a\'b' || 'c\'`:                     {"a'b", `c\`},
		`$$body$$ || $fn$ 'not' $fn$ || $1`:   {"body", " 'not' "},
		`x = $1 AND y = $2`:                   nil,
		`'unterminated`:                       {"unterminated"},
		`"quoted ident" = 'v'`:                {"v"},
		``:                                    nil,
		`(state = 'secret-state-42'::text)`:   {"secret-state-42"},
		`$tag$never closed`:                   {"never closed"},
		`DATE'2026-01-01'`:                    {"2026-01-01"},
		`ce'x\'`:                              {`x\`},
		`'a'||$q$b$q$`:                        {"a", "b"},
		`WHERE note = 'x' -- comment 'later'`: {"x", "later"},
	} {
		if got := stringLiterals(in); !reflect.DeepEqual(got, want) {
			t.Errorf("stringLiterals(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWordsOutsideLiterals_SkipsLiteralBodies(t *testing.T) {
	got := wordsOutsideLiterals(`SELECT "My T".x FROM t WHERE a = 'secret' AND b = $1`)
	want := []string{"SELECT", "My T", "x", "FROM", "t", "WHERE", "a", "AND", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("words = %q, want %q", got, want)
	}
}

func TestPGArrayElements_QuotedNullAndMalformed(t *testing.T) {
	for in, want := range map[string][]string{
		`{a,b}`:                      {"a", "b"},
		`{"c d","e\"f",NULL,"NULL"}`: {"c d", `e"f`, "NULL"},
		`{}`:                         nil,
		``:                           nil,
		`not an array`:               nil,
		`{ spaced , x }`:             {"spaced", "x"},
	} {
		if got := pgArrayElements(in); !reflect.DeepEqual(got, want) {
			t.Errorf("pgArrayElements(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeakNeedles_ShortNumericAndNameValuesAreNotNeedles(t *testing.T) {
	local := map[string]any{fileStats: map[string]any{"columns": []any{
		map[string]any{"most_common_vals": `{open,123456789,orders,unknown,long-marker}`},
	}}}
	idx := leakNeedles(local, map[string]bool{"orders": true}, map[string]any{})
	var got []string
	for _, ns := range idx {
		for _, n := range ns {
			got = append(got, n.value)
		}
	}
	if !reflect.DeepEqual(got, []string{"long-marker"}) {
		t.Fatalf("needles = %q, want only long-marker", got)
	}
}
