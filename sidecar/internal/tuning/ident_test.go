package tuning

import "testing"

// No concurrent access tests: the identifier helpers are pure functions.

// canonicalRef folds bare names and renders each part with sqlident, so a
// table named like any key word that needs quoting has one canonical form
// whether it was written bare-folded or quoted.
func TestCanonicalRef(t *testing.T) {
	cases := map[string]string{
		"public.orders":           "public.orders",
		"Public.Orders":           "public.orders",
		` public . "Orders" `:     `public."Orders"`,
		`public."order"`:          `public."order"`,
		`public."left"`:           `public."left"`,
		`public."time"`:           `public."time"`,
		`"App"."we""ird"`:         `"App"."we""ird"`,
		`public."user_id"`:        "public.user_id",
		"orders":                  "",
		"public.orders extra":     "",
		`public.""`:               "",
		"":                        "",
		"public.":                 "",
		`public."unterminated`:    "",
		"1schema.t":               "",
		"public.index":            "public.index",
		`"sales"."values"`:        `sales."values"`,
		"sales.\"Line Items\"   ": `sales."Line Items"`,
	}
	for in, want := range cases {
		if got := canonicalRef(in); got != want {
			t.Errorf("canonicalRef(%q) = %q, want %q", in, got, want)
		}
	}
}
