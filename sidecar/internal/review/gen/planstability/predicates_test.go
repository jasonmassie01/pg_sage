package planstability

import (
	"reflect"
	"strings"
	"testing"
)

func TestPredicates(t *testing.T) {
	cases := []struct {
		text string
		want []predicate
	}{
		{"SELECT id FROM public.users WHERE deleted_at IS NOT NULL",
			[]predicate{{column: "deleted_at", op: opNotNull}}},
		{"select * from users u where u.Status = $1 and $2 = u.region",
			[]predicate{{qualifier: "u", column: "status", op: opEqual},
				{qualifier: "u", column: "region", op: opEqual}}},
		{`SELECT 1 FROM t WHERE "Mixed""Col" = 'x' AND a >= $1 AND b < 3 AND c <= now()`,
			[]predicate{{column: `Mixed"Col`, op: opEqual}, {column: "a", op: opRange},
				{column: "b", op: opRange}, {column: "c", op: opRange}}},
		// A join condition compares two columns: neither is a filter.
		{"SELECT 1 FROM a JOIN b ON a.id = b.a_id WHERE b.k = $1",
			[]predicate{{qualifier: "b", column: "k", op: opEqual}}},
		// A column compared inside a literal or comment is not a predicate.
		{"SELECT 'x = 1' FROM t /* y = 2 */ WHERE z IS NULL", nil},
		{"SELECT 1 FROM t WHERE x <> $1 AND y != 2", nil},
		{"SELECT 1 FROM t WHERE x = ANY($1)", nil},
	}
	for _, tc := range cases {
		got, err := predicates(tc.text)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("predicates(%q) = %+v, %v; want %+v", tc.text, got, err, tc.want)
		}
	}
}

func TestPredicates_Unparseable(t *testing.T) {
	for _, text := range []string{"", "   ", "SELECT 'open", `SELECT "open`,
		"SELECT 1; SELECT 2", "SELECT /* open"} {
		if got, err := predicates(text); err == nil {
			t.Errorf("predicates(%q) = %+v, want a reason", text, got)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("predicates(%q): empty reason", text)
		}
	}
}
