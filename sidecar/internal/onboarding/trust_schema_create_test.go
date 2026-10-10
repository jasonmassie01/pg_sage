package onboarding

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// CREATE INDEX and CREATE STATISTICS need CREATE on the table's schema even
// for the table's owner, so the levels that run them (advisory, autonomous)
// list that grant; observation runs no DDL and does not.

func withSchemaCreate(sc *rolegrants.SchemaCreate) GuideInput {
	in := guideInput("observation")
	in.Grants.SchemaCreate = sc
	return in
}

func TestSchemaCreateGrantOnExecutingLevelsOnly(t *testing.T) {
	sc := &rolegrants.SchemaCreate{Schemas: []string{"app", "public"},
		Missing: []string{"app"}}
	levels := TrustGuide(withSchemaCreate(sc))
	if g := grant(level(levels, LevelObservation), GrantSchemaCreate); g.Name != "" {
		t.Fatalf("observation lists %+v, want no schema CREATE grant", g)
	}
	for _, name := range []string{LevelAdvisory, LevelAutonomous} {
		g := grant(level(levels, name), GrantSchemaCreate)
		if g.Present == nil || *g.Present {
			t.Fatalf("%s schema grant = %+v, want checked and missing", name, g)
		}
		if g.SQL != "GRANT CREATE ON SCHEMA app TO sage_agent;" {
			t.Fatalf("%s SQL = %q, want the missing schema only", name, g.SQL)
		}
		if !strings.Contains(g.Why, "CREATE INDEX") ||
			!strings.Contains(g.Why, "CREATE STATISTICS") {
			t.Fatalf("%s why = %q, want the statements that need it", name, g.Why)
		}
		if !strings.Contains(g.Detail, "1 of 2") || !strings.Contains(g.Detail, "app") {
			t.Fatalf("%s detail = %q, want 1 of 2 schemas and the missing one", name,
				g.Detail)
		}
	}
}

// The schema grant sits next to table ownership, the other half of what
// CREATE INDEX needs.
func TestSchemaCreateGrantFollowsTableOwnership(t *testing.T) {
	sc := &rolegrants.SchemaCreate{Schemas: []string{"app"}}
	adv := level(TrustGuide(withSchemaCreate(sc)), LevelAdvisory)
	for i, g := range adv.Grants {
		if g.Name == GrantTableOwnership {
			if i+1 >= len(adv.Grants) || adv.Grants[i+1].Name != GrantSchemaCreate {
				t.Fatalf("grants = %+v, want schema CREATE right after ownership", adv.Grants)
			}
			return
		}
	}
	t.Fatalf("grants = %+v, want table ownership listed", adv.Grants)
}

func TestSchemaCreateGrantPresent(t *testing.T) {
	sc := &rolegrants.SchemaCreate{Schemas: []string{"app", "public"}}
	g := grant(level(TrustGuide(withSchemaCreate(sc)), LevelAutonomous), GrantSchemaCreate)
	if g.Present == nil || !*g.Present || !strings.Contains(g.Detail, "2 of 2") ||
		g.SQL != "GRANT CREATE ON SCHEMA app, public TO sage_agent;" {
		t.Fatalf("schema grant = %+v, want present on 2 of 2 schemas", g)
	}
}

// Nil: not checked. Empty: no user tables yet, nothing to check; both show
// the grant for public without claiming it present or missing.
func TestSchemaCreateGrantUnknown(t *testing.T) {
	for name, sc := range map[string]*rolegrants.SchemaCreate{"nil": nil,
		"no tables": {}} {
		g := grant(level(TrustGuide(withSchemaCreate(sc)), LevelAdvisory), GrantSchemaCreate)
		if g.Name != GrantSchemaCreate || g.Present != nil ||
			g.SQL != "GRANT CREATE ON SCHEMA public TO sage_agent;" {
			t.Fatalf("%s: schema grant = %+v, want unknown, for public", name, g)
		}
	}
}

// A schema-per-tenant database can lack CREATE on hundreds of schemas: the
// SQL names every one, the one-line detail the first ten and a count.
func TestSchemaCreateGrantDetailNamesTenSchemas(t *testing.T) {
	for _, missing := range []int{9, 10, 11, 250} {
		var names []string
		for i := range missing {
			names = append(names, fmt.Sprintf("tenant_%03d", i))
		}
		sc := &rolegrants.SchemaCreate{Schemas: append([]string{"public"}, names...),
			Missing: names}
		g := grant(level(TrustGuide(withSchemaCreate(sc)), LevelAdvisory), GrantSchemaCreate)
		wantSQL := "GRANT CREATE ON SCHEMA " + strings.Join(names, ", ") + " TO sage_agent;"
		if g.SQL != wantSQL {
			t.Fatalf("%d missing: SQL = %q, want every missing schema", missing, g.SQL)
		}
		shown := min(missing, 10)
		want := fmt.Sprintf("CREATE on 1 of %d schemas with tables; missing: %s",
			missing+1, strings.Join(names[:shown], ", "))
		if missing > shown {
			want += fmt.Sprintf(" and %d more", missing-shown)
		}
		if g.Detail != want {
			t.Fatalf("%d missing: detail = %q, want %q", missing, g.Detail, want)
		}
	}
}

func TestSchemaCreateGrantQuotesRoleAndSchemas(t *testing.T) {
	in := withSchemaCreate(&rolegrants.SchemaCreate{Schemas: []string{`"My App"`, `"order"`},
		Missing: []string{`"My App"`, `"order"`}})
	in.Grants.Role = `Sage "Agent"`
	g := grant(level(TrustGuide(in), LevelAdvisory), GrantSchemaCreate)
	want := `GRANT CREATE ON SCHEMA "My App", "order" TO "Sage ""Agent""";`
	if g.SQL != want {
		t.Fatalf("SQL = %q, want %q", g.SQL, want)
	}
}
