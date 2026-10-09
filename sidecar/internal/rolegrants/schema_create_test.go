package rolegrants

import (
	"strings"
	"testing"
)

// No concurrent access tests: SchemaCreate is a value and its methods are
// pure; CheckSchemaCreate shares nothing between calls.

func TestSchemaCreateLacking(t *testing.T) {
	cases := []struct {
		name string
		s    SchemaCreate
		want bool
	}{
		{"zero value", SchemaCreate{}, false},
		{"all granted", SchemaCreate{Schemas: []string{"app", "public"}}, false},
		{"one missing", SchemaCreate{Schemas: []string{"app", "public"},
			Missing: []string{"app"}}, true},
	}
	for _, c := range cases {
		if got := c.s.Lacking(); got != c.want {
			t.Errorf("%s: Lacking() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSchemaCreateGrantSQLNamesOnlyMissingSchemas(t *testing.T) {
	s := SchemaCreate{Schemas: []string{`"My App"`, "app", "public"},
		Missing: []string{`"My App"`, "public"}}
	want := `GRANT CREATE ON SCHEMA "My App", public TO sage_agent`
	if got := s.GrantSQL("sage_agent"); got != want {
		t.Fatalf("GrantSQL = %q, want %q", got, want)
	}
}

// Nothing missing: the SQL still names every schema holding tables, so the
// guide shows what the grant is even when it is already in place.
func TestSchemaCreateGrantSQLAllGranted(t *testing.T) {
	s := SchemaCreate{Schemas: []string{"app", "public"}}
	want := "GRANT CREATE ON SCHEMA app, public TO sage_agent"
	if got := s.GrantSQL("sage_agent"); got != want {
		t.Fatalf("GrantSQL = %q, want %q", got, want)
	}
}

// Empty: no user tables yet, so the grant names public, where new tables
// go by default.
func TestSchemaCreateGrantSQLNoTables(t *testing.T) {
	want := `GRANT CREATE ON SCHEMA public TO "Sage"`
	if got := (SchemaCreate{}).GrantSQL(`"Sage"`); got != want {
		t.Fatalf("GrantSQL = %q, want %q", got, want)
	}
}

func TestSchemaCreateWhat(t *testing.T) {
	cases := []struct {
		s    SchemaCreate
		want string
	}{
		{SchemaCreate{Schemas: []string{"app", "public"}, Missing: []string{"public"}},
			"CREATE on schema public"},
		{SchemaCreate{Schemas: []string{"app", `"order"`},
			Missing: []string{"app", `"order"`}}, `CREATE on schemas app, "order"`},
		{SchemaCreate{Schemas: []string{"app"}}, "CREATE on schema app"},
		{SchemaCreate{}, "CREATE on schema public"},
	}
	for _, c := range cases {
		if got := c.s.What(); got != c.want {
			t.Errorf("What(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestSchemaCreateNeedNamesTheStatements(t *testing.T) {
	if !strings.Contains(Need, "CREATE INDEX") || !strings.Contains(Need, "CREATE STATISTICS") {
		t.Fatalf("Need = %q, want both statements named", Need)
	}
}
