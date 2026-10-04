package extstats

import (
	"strings"
	"testing"
)

// Owner decision 2026-10-04 (PR #110): pg_sage may run CREATE STATISTICS,
// but only one form: a pg_sage-named object in the table's schema, of the
// kinds the verifier understands, on plain columns of one table. Its
// inverse is DROP STATISTICS IF EXISTS of exactly that object.

func TestParseCreateAcceptsThePgSageForm(t *testing.T) {
	c, err := ParseCreate("CREATE STATISTICS public.sage_stx_orders_ab " +
		"(ndistinct, dependencies, mcv) ON a, b FROM public.orders")
	if err != nil {
		t.Fatalf("ParseCreate: %v", err)
	}
	if c.Schema.Name != "public" || c.Name.Name != "sage_stx_orders_ab" ||
		c.TableSchema.Name != "public" || c.Table.Name != "orders" {
		t.Fatalf("parsed %+v", c)
	}
	if strings.Join(c.Kinds, ",") != "ndistinct,dependencies,mcv" {
		t.Fatalf("kinds = %v", c.Kinds)
	}
	if strings.Join(c.Columns, ",") != "a,b" {
		t.Fatalf("columns = %v", c.Columns)
	}
	if c.IfNotExists {
		t.Fatal("IfNotExists set for a plain CREATE")
	}
}

func TestParseCreateVariants(t *testing.T) {
	for sql, want := range map[string]string{
		// no kind list: PostgreSQL builds every kind
		"CREATE STATISTICS app.sage_stx_x ON a, b FROM app.t":                      "app.sage_stx_x|app.t|",
		"create statistics if not exists App.SAGE_STX_X (mcv) on A, B from APP.T;": "App.SAGE_STX_X|APP.T|mcv",
		"CREATE   STATISTICS\n\t\"Sales\".sage_stx_y (dependencies)  ON \"Col\", b " +
			"FROM \"Sales\".\"Orders\"": `"Sales".sage_stx_y|"Sales"."Orders"|dependencies`,
	} {
		c, err := ParseCreate(sql)
		if err != nil {
			t.Errorf("ParseCreate(%q): %v", sql, err)
			continue
		}
		got := c.QualifiedName() + "|" + c.QualifiedTable() + "|" + strings.Join(c.Kinds, ",")
		if got != want {
			t.Errorf("ParseCreate(%q) = %s, want %s", sql, got, want)
		}
	}
}

func TestParseCreateCanonicalNames(t *testing.T) {
	c, err := ParseCreate(`CREATE STATISTICS IF NOT EXISTS "Sales".SAGE_STX_Y ON "Col", B ` +
		`FROM "Sales"."Orders"`)
	if err != nil {
		t.Fatalf("ParseCreate: %v", err)
	}
	if !c.IfNotExists || c.Schema.Name != "Sales" || c.Name.Name != "sage_stx_y" ||
		c.Table.Name != "Orders" || strings.Join(c.Columns, ",") != "Col,b" {
		t.Fatalf("canonical names = %+v", c)
	}
}

func TestParseCreateRefusesOtherForms(t *testing.T) {
	for sql, want := range map[string]string{
		"":                               "not a CREATE STATISTICS",
		"CREATE INDEX i ON public.t (a)": "not a CREATE STATISTICS",
		"CREATE STATISTICS sage_stx_x ON a, b FROM public.t":                             "schema-qualified",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM t":                             "schema-qualified",
		"CREATE STATISTICS public.st_orders ON a, b FROM public.t":                       "sage_stx_",
		`CREATE STATISTICS public."Sage_Stx_x" ON a, b FROM public.t`:                    "sage_stx_",
		"CREATE STATISTICS app.sage_stx_x ON a, b FROM public.t":                         "table's schema",
		"CREATE STATISTICS public.sage_stx_x (expressions) ON a, b FROM public.t":        "kind",
		"CREATE STATISTICS public.sage_stx_x (mcv, mcv) ON a, b FROM public.t":           "twice",
		"CREATE STATISTICS public.sage_stx_x () ON a, b FROM public.t":                   "kind",
		"CREATE STATISTICS public.sage_stx_x ON a FROM public.t":                         "2 to 8 columns",
		"CREATE STATISTICS public.sage_stx_x ON a, b, c, d, e, f, g, h, i FROM public.t": "2 to 8",
		"CREATE STATISTICS public.sage_stx_x ON a, a FROM public.t":                      "twice",
		"CREATE STATISTICS public.sage_stx_x ON A, a FROM public.t":                      "twice",
		"CREATE STATISTICS public.sage_stx_x ON (lower(a)), b FROM public.t":             "column",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t, public.u":            "one table",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t; DROP TABLE public.t": "one table",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t -- c":                 "one table",
		"CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t WHERE a > 1":          "one table",
		`CREATE STATISTICS public.sage_stx_x ON "a b", c FROM public.t`:                  "column",
		"CREATE STATISTICS public.sage_stx_" + strings.Repeat("x", 60) +
			" ON a, b FROM public.t": "63",
	} {
		_, err := ParseCreate(sql)
		if err == nil {
			t.Errorf("ParseCreate(%q) accepted, want refusal mentioning %q", sql, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseCreate(%q) = %v, want it to mention %q", sql, err, want)
		}
	}
}

// Exactly two and exactly eight columns are the boundaries PostgreSQL and
// the rule share.
func TestParseCreateColumnBoundaries(t *testing.T) {
	for _, cols := range []string{"a, b", "a, b, c, d, e, f, g, h"} {
		if _, err := ParseCreate("CREATE STATISTICS public.sage_stx_x ON " + cols +
			" FROM public.t"); err != nil {
			t.Errorf("columns %q refused: %v", cols, err)
		}
	}
}

func TestRollbackAndAnalyzeReuseTheWrittenNames(t *testing.T) {
	c, err := ParseCreate(`CREATE STATISTICS "Sales".sage_stx_y (dependencies) ON a, b ` +
		`FROM "Sales"."Orders"`)
	if err != nil {
		t.Fatalf("ParseCreate: %v", err)
	}
	if got := c.Rollback(); got != `DROP STATISTICS IF EXISTS "Sales".sage_stx_y` {
		t.Fatalf("Rollback = %q", got)
	}
	if got := c.Analyze(); got != `ANALYZE "Sales"."Orders"` {
		t.Fatalf("Analyze = %q", got)
	}
}

func TestParseDrop(t *testing.T) {
	for sql, want := range map[string]string{
		"DROP STATISTICS IF EXISTS public.sage_stx_x":  "public.sage_stx_x|true",
		"drop statistics App.SAGE_STX_X;":              "App.SAGE_STX_X|false",
		`DROP STATISTICS IF EXISTS "Sales".sage_stx_y`: `"Sales".sage_stx_y|true`,
	} {
		d, err := ParseDrop(sql)
		if err != nil {
			t.Errorf("ParseDrop(%q): %v", sql, err)
			continue
		}
		got := d.QualifiedName() + "|" + map[bool]string{true: "true", false: "false"}[d.IfExists]
		if got != want {
			t.Errorf("ParseDrop(%q) = %s, want %s", sql, got, want)
		}
	}
}

func TestParseDropRefusesOtherForms(t *testing.T) {
	for sql, want := range map[string]string{
		"DROP INDEX public.sage_stx_x":                           "not a DROP STATISTICS",
		"DROP STATISTICS sage_stx_x":                             "schema-qualified",
		"DROP STATISTICS public.st_orders":                       "sage_stx_",
		"DROP STATISTICS public.sage_stx_x CASCADE":              "one statistics object",
		"DROP STATISTICS public.sage_stx_x RESTRICT":             "one statistics object",
		"DROP STATISTICS public.sage_stx_x, public.sage_stx_y":   "one statistics object",
		"DROP STATISTICS public.sage_stx_x; DROP TABLE public.t": "one statistics object",
		"DROP STATISTICS IF EXISTS pg_catalog.sage_stx_x extra":  "one statistics object",
	} {
		_, err := ParseDrop(sql)
		if err == nil {
			t.Errorf("ParseDrop(%q) accepted, want refusal mentioning %q", sql, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseDrop(%q) = %v, want it to mention %q", sql, err, want)
		}
	}
}

func TestUndoesMatchesOnlyTheCreatedObject(t *testing.T) {
	c, err := ParseCreate("CREATE STATISTICS public.sage_stx_x ON a, b FROM public.t")
	if err != nil {
		t.Fatalf("ParseCreate: %v", err)
	}
	for drop, want := range map[string]bool{
		"DROP STATISTICS IF EXISTS public.sage_stx_x":         true,
		`DROP STATISTICS "public"."sage_stx_x"`:               true,
		"DROP STATISTICS IF EXISTS PUBLIC.SAGE_STX_X":         true,
		"DROP STATISTICS IF EXISTS public.sage_stx_y":         false,
		"DROP STATISTICS IF EXISTS app.sage_stx_x":            false,
		`DROP STATISTICS IF EXISTS public."SAGE_STX_X"`:       false,
		"DROP INDEX CONCURRENTLY IF EXISTS public.sage_stx_x": false,
		"": false,
	} {
		if got := Undoes(c, drop); got != want {
			t.Errorf("Undoes(%q) = %v, want %v", drop, got, want)
		}
	}
}

func TestOwnName(t *testing.T) {
	for name, want := range map[string]bool{
		"sage_stx_orders_ab":                  true,
		"sage_stx_":                           false, // the prefix alone names nothing
		"st_orders":                           false,
		"Sage_Stx_x":                          false, // canonical names are case-exact
		"x_sage_stx_y":                        false,
		"sage_stx_" + strings.Repeat("x", 54): true,  // 63 bytes
		"sage_stx_" + strings.Repeat("x", 55): false, // 64 bytes: PostgreSQL truncates
	} {
		if got := OwnName(name); got != want {
			t.Errorf("OwnName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsCreateAndIsDrop(t *testing.T) {
	if !IsCreate("  create statistics public.x ON a, b FROM public.t") ||
		IsCreate("CREATE INDEX i ON public.t (a)") || IsCreate("CREATE STATISTICSX") {
		t.Fatal("IsCreate misclassified")
	}
	if !IsDrop("DROP STATISTICS IF EXISTS public.x") || IsDrop("DROP INDEX public.x") {
		t.Fatal("IsDrop misclassified")
	}
}
