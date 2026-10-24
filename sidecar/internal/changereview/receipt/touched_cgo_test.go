//go:build cgo

package receipt

import (
	"fmt"
	"testing"
)

// Born queryids are matched to touched relations by their parse tree; a
// statement the parser rejects falls back to a whole-word name match.
func TestTouchedRelations(t *testing.T) {
	rels := []string{"rv.rv_orders", "public.items"}
	cases := []struct {
		text string
		want []string
	}{
		{"SELECT status FROM rv.rv_orders WHERE id = $1", []string{"rv.rv_orders"}},
		{"SELECT * FROM items i JOIN rv.rv_orders o ON o.id = i.order_id",
			[]string{"rv.rv_orders", "public.items"}},
		{"SELECT 1 FROM rv.rv_orders_archive", nil},
		{"SELECT 1 FROM other.rv_orders", nil},
		// The parser rejects it; the name match still finds the table.
		{"SELECT FROM WHERE rv_orders (", []string{"rv.rv_orders"}},
		{"not sql mentioning items_old", nil},
	}
	for _, c := range cases {
		got := touched(c.text, rels)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("touched(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	if touched("SELECT 1", []string{"unqualified"}) != nil {
		t.Error("a relation without a schema matched")
	}
}
