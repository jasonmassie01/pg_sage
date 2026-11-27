package explain

import (
	"errors"
	"strings"
	"testing"
)

// No concurrent access tests: paramPlaceholders is a pure function that
// scans its argument with a fresh scanner on every call.

// paramPlaceholders reports the highest $N bind parameter that PostgreSQL
// itself would see: text inside literals, quoted identifiers, dollar
// quotes and comments is not a parameter.
func TestParamPlaceholders(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"single", "SELECT * FROM t WHERE id = $1", 1},
		{"two", "SELECT * FROM t WHERE a = $1 AND b = $2", 2},
		{"highest wins with gaps", "SELECT * FROM t WHERE a = $3", 3},
		{"repeated", "SELECT $1, $1, $1", 1},
		{"double digit", "SELECT $10, $2", 10},
		{"nine", "SELECT $9", 9},
		{"no params", "SELECT * FROM t", 0},
		{"empty", "", 0},
		{"whitespace only", " \t\n", 0},
		{"cast after param", "SELECT $1::int", 1},
		{"param in parentheses", "SELECT ($2)", 2},
		{"string literal", "SELECT '$5'", 0},
		{"dollar-quoted body", "SELECT $q$ $1 $q$", 0},
		{"anonymous dollar quote", "SELECT $$ $7 $$", 0},
		{"param and literal", "SELECT $1, '$2'", 1},
		{"quoted identifier", `SELECT 1 AS "$3"`, 0},
		{"line comment", "SELECT 1 -- $4", 0},
		{"line comment then param", "SELECT 1 -- $4\n, $2", 2},
		{"block comment", "SELECT /* $4 */ 1", 0},
		{"nested block comment", "SELECT /* a /* b */ $4 */ 1", 0},
		{"hint comment", "/*+ SeqScan(t) */ SELECT $1 FROM t", 1},
		{"escape string", `SELECT E'\'$4'`, 0},
		{"doubled quote", "SELECT 'it''s $6'", 0},
		{"identifier with dollar", "SELECT foo$1bar FROM t", 0},
		{"dollar then letter", "SELECT $a", 0},
		{"lone dollar", "SELECT $", 0},
		{"upper bound", "SELECT $65535", 65535},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paramPlaceholders(tt.query)
			if err != nil {
				t.Fatalf("paramPlaceholders(%q) error: %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("paramPlaceholders(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

// Inputs PostgreSQL would reject, or that would make EXECUTE allocate an
// argument list of absurd size, are client errors.
func TestParamPlaceholders_Invalid(t *testing.T) {
	tests := []struct {
		name, query, wantMsg string
	}{
		{"zero", "SELECT $0", "$0"},
		{"past the protocol limit", "SELECT $65536", "65535"},
		{"overflows int", "SELECT $99999999999999999999", "65535"},
		{"unterminated literal", "SELECT '$1", "unterminated"},
		{"unterminated dollar quote", "SELECT $$", "unterminated"},
		{"unterminated comment", "SELECT 1 /* $1", "unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paramPlaceholders(tt.query)
			if err == nil {
				t.Fatalf("paramPlaceholders(%q) = %d, want an error", tt.query, got)
			}
			if !errors.Is(err, ErrExplainInvalidRequest) {
				t.Errorf("error %v does not wrap ErrExplainInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
			if got != 0 {
				t.Errorf("count on error = %d, want 0", got)
			}
		})
	}
}

// EXECUTE gets one argument per placeholder the server sees: a literal
// that looks like "$2" adds no NULL argument.
func TestExplainParamList_IgnoresLiteralPlaceholders(t *testing.T) {
	got, err := explainParamList(1, []string{"7"})
	if err != nil || got != "'7'" {
		t.Fatalf("explainParamList(1, [7]) = %q, %v; want '7'", got, err)
	}
	got, err = explainParamList(3, []string{"a", ""})
	if err != nil || got != "'a', NULL, NULL" {
		t.Fatalf("explainParamList(3, [a ]) = %q, %v", got, err)
	}
	if got, err = explainParamList(0, []string{"x"}); err != nil || got != "" {
		t.Fatalf("explainParamList(0, [x]) = %q, %v; want empty", got, err)
	}
	if _, err = explainParamList(1, []string{"a\x00"}); !errors.Is(err, ErrExplainInvalidRequest) {
		t.Fatalf("NUL parameter error = %v, want ErrExplainInvalidRequest", err)
	}
}

func TestHasUnboundParam_Count(t *testing.T) {
	cases := []struct {
		n      int
		params []string
		want   bool
	}{
		{0, nil, false},
		{1, nil, true},
		{1, []string{"1"}, false},
		{2, []string{"1"}, true},
		{2, []string{"1", ""}, true},
		{1, []string{"1", "extra"}, false},
	}
	for _, c := range cases {
		if got := hasUnboundParam(c.n, c.params); got != c.want {
			t.Errorf("hasUnboundParam(%d, %q) = %v, want %v", c.n, c.params, got, c.want)
		}
	}
}
