package shadow

import (
	"slices"
	"testing"
)

// Unit (no database), earned-03: what the scorer looks for in pg_sage's
// own index actions. No concurrent access tests: both functions are pure.

func TestOwnActionNeedle(t *testing.T) {
	for _, tc := range []struct {
		name, class, sql, want string
		ok                     bool
	}{
		{"create names its table", "index_create",
			`CREATE INDEX CONCURRENTLY i ON public.orders (a)`, "orders", true},
		{"unqualified table", "index_create", `CREATE INDEX i ON orders (a)`, "orders", true},
		{"drop names its index", "index_drop",
			`DROP INDEX CONCURRENTLY IF EXISTS app.orders_a_idx`, `orders\_a\_idx`, true},
		{"quoted table keeps its dot", "index_create",
			`CREATE INDEX i ON public."my.orders" (a)`, "my.orders", true},
		{"like wildcards are escaped", "index_create",
			`CREATE INDEX i ON public."50%_off\x" (a)`, `50\%\_off\\x`, true},
		{"doubled quote stays doubled", "index_create",
			`CREATE INDEX i ON "a""b" (a)`, `a""b`, true},
		{"empty shape", "index_create", ``, "", false},
		{"create without a table", "index_create", `CREATE INDEX i`, "", false},
		{"drop without a name", "index_drop", `DROP INDEX`, "", false},
		{"other class", "vacuum", `VACUUM public.orders`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ownActionNeedle(Decision{Class: tc.class, Shape: Shape(tc.sql)})
			if got != tc.want || ok != tc.ok {
				t.Fatalf("needle(%q) = %q, %v; want %q, %v", Shape(tc.sql), got, ok, tc.want,
					tc.ok)
			}
		})
	}
}

func TestActionShapes(t *testing.T) {
	create := `CREATE INDEX CONCURRENTLY n ON public.o (a, b)`
	drop := `DROP INDEX CONCURRENTLY public.old`
	for _, tc := range []struct {
		name, actionType, sql string
		want                  []string
	}{
		{"create", "create_index", create + ";", []string{Shape(create)}},
		{"drop", "drop_index", drop, []string{Shape(drop)}},
		{"replace is both statements", "replace_index", create + "; " + drop + ";",
			[]string{Shape(create), Shape(drop)}},
		{"malformed replace is one statement", "replace_index", create,
			[]string{Shape(create)}},
		{"empty", "create_index", "", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionShapes(tc.actionType, tc.sql); !slices.Equal(got, tc.want) {
				t.Fatalf("shapes = %q, want %q", got, tc.want)
			}
		})
	}
}
