package impact

import "testing"

// Stateless: no concurrent-access test applies.
func TestDenseParamsRenumbersInOrderOfUse(t *testing.T) {
	const w = "SELECT 1 FROM t WHERE "
	cases := []struct{ in, want string }{
		{w + "b = $2", w + "b = $1"},
		{w + "b = $3 AND c = $2", w + "b = $1 AND c = $2"},
		{w + "b = $2 OR b > $2", w + "b = $1 OR b > $1"},
		{w + "b = '$2' AND c = $4", w + "b = '$2' AND c = $1"},
		{`SELECT "$9" FROM t WHERE c = $10`, `SELECT "$9" FROM t WHERE c = $1`},
		{w + "b = 'it''s $3' AND $5", w + "b = 'it''s $3' AND $1"},
		{"SELECT 1", "SELECT 1"},
		{"", ""},
		{"SELECT 'unterminated $2", "SELECT 'unterminated $2"},
		{"SELECT price $ FROM t WHERE a = $1", "SELECT price $ FROM t WHERE a = $1"},
		{w + "x = $12 AND y = $1", w + "x = $1 AND y = $2"},
		{w + "b = $2 AND c = $2::text", w + "b = $1 AND c = $1::text"},
	}
	for _, c := range cases {
		if got := denseParams(c.in); got != c.want {
			t.Errorf("denseParams(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
