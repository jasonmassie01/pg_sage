package onboarding

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func execAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// ownerRole creates a NOLOGIN role owning n new tables in schema.
func ownerRole(t *testing.T, ctx context.Context, admin *pgxpool.Pool, name, schema string,
	n int) {
	t.Helper()
	rq := pgx.Identifier{name}.Sanitize()
	execAll(t, ctx, admin, "CREATE ROLE "+rq+" NOLOGIN",
		"CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize(),
		fmt.Sprintf(`DO $$ BEGIN FOR i IN 1..%d LOOP
  EXECUTE format('CREATE TABLE %%I.%%I (id int)', %s, 't' || i);
  EXECUTE format('ALTER TABLE %%I.%%I OWNER TO %%I', %s, 't' || i, %s);
END LOOP; END $$`, n, quoteLit(schema), quoteLit(schema), quoteLit(name)))
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = admin.Exec(bg, "DROP OWNED BY "+rq)
		_, _ = admin.Exec(bg, "DROP ROLE IF EXISTS "+rq)
	})
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func uniq(prefix string) string {
	return fmt.Sprintf("%s_%06x", prefix, time.Now().UnixNano()&0xffffff)
}

// Integration: tables owned by two roles give two grant lines naming each
// role and its count; running them gives pg_sage the owners' privileges.
func TestCheckGrantsListsTableOwners(t *testing.T) {
	admin, ctx := freshInstall(t)
	big, small := uniq("own_big"), uniq("own_small")
	ownerRole(t, ctx, admin, big, "bigapp", 412)
	ownerRole(t, ctx, admin, small, "smallapp", 3)
	mon := monitorPool(t, ctx, admin)
	g, err := CheckGrants(ctx, mon)
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	want := []TableOwner{{Role: big, Tables: 412}, {Role: small, Tables: 3}}
	if !reflect.DeepEqual(g.Owners, want) || g.OwnerRoles != 2 || g.TablesTotal != 415 {
		t.Fatalf("owners = %+v (%d roles, %d tables), want %+v", g.Owners, g.OwnerRoles,
			g.TablesTotal, want)
	}
	lines := ownerLines(level(TrustGuide(GuideInput{Current: LevelObservation, Grants: g}),
		LevelAdvisory))
	if len(lines) != 2 {
		t.Fatalf("owner lines = %+v, want two", lines)
	}
	for i, o := range want {
		l := lines[i]
		if !strings.Contains(l.Detail, o.Role) || !strings.Contains(l.Detail,
			fmt.Sprint(o.Tables)) || !strings.Contains(l.Why, "DROP") {
			t.Fatalf("line %d = %+v, want %s, %d tables and DROP", i, l, o.Role, o.Tables)
		}
		execAll(t, ctx, admin, l.SQL)
	}
	after, err := CheckGrants(ctx, mon)
	if err != nil {
		t.Fatalf("check after grant: %v", err)
	}
	if len(after.Owners) != 0 || after.TablesOwned != 415 {
		t.Fatalf("after grant: owners %+v, owned %d of %d", after.Owners, after.TablesOwned,
			after.TablesTotal)
	}
	own := ownershipAny(level(TrustGuide(GuideInput{Current: LevelObservation,
		Grants: after}), LevelAdvisory))
	if len(own) != 1 || own[0].Present == nil || !*own[0].Present {
		t.Fatalf("after grant: ownership = %+v, want one present grant", own)
	}
}

// A superuser owner is listed as such and gets reassign advice.
func TestCheckGrantsSuperuserOwner(t *testing.T) {
	admin, ctx := freshInstall(t)
	execAll(t, ctx, admin, "CREATE TABLE public.su_owned (id int)")
	var super bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = "+
		"current_user").Scan(&super); err != nil || !super {
		t.Fatalf("admin is not a superuser (%v): this test needs one", err)
	}
	g, err := CheckGrants(ctx, monitorPool(t, ctx, admin))
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	if len(g.Owners) != 1 || !g.Owners[0].Superuser || g.Owners[0].Tables != 1 {
		t.Fatalf("owners = %+v, want the superuser owning one table", g.Owners)
	}
	lines := ownerLines(level(TrustGuide(GuideInput{Grants: g}), LevelAdvisory))
	if len(lines) != 1 || strings.Contains(lines[0].SQL, "GRANT") ||
		!strings.Contains(strings.ToLower(lines[0].Why), "reassign") {
		t.Fatalf("superuser owner lines = %+v, want reassign advice, no GRANT", lines)
	}
}

// Invalid input: an owner role whose name needs quoting gets SQL that runs.
func TestCheckGrantsQuotedOwner(t *testing.T) {
	admin, ctx := freshInstall(t)
	name := `we"ird` + uniq("")
	ownerRole(t, ctx, admin, name, "weird", 1)
	mon := monitorPool(t, ctx, admin)
	g, err := CheckGrants(ctx, mon)
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	lines := ownerLines(level(TrustGuide(GuideInput{Grants: g}), LevelAdvisory))
	if len(lines) != 1 || !strings.HasPrefix(lines[0].SQL,
		`GRANT "we""ird`) {
		t.Fatalf("lines = %+v, want the quoted owner role", lines)
	}
	execAll(t, ctx, admin, lines[0].SQL)
	if after, err := CheckGrants(ctx, mon); err != nil || len(after.Owners) != 0 {
		t.Fatalf("after grant: %+v, %v", after.Owners, err)
	}
}

// Boundary: the owner list stops at 20 roles and counts the rest.
func TestCheckGrantsOwnerListBounded(t *testing.T) {
	admin, ctx := freshInstall(t)
	for i := 0; i < 21; i++ {
		ownerRole(t, ctx, admin, fmt.Sprintf("%s_%02d", uniq("own"), i),
			fmt.Sprintf("many_%02d", i), 1)
	}
	g, err := CheckGrants(ctx, monitorPool(t, ctx, admin))
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	if len(g.Owners) != 20 || g.OwnerRoles != 21 || g.TablesTotal != 21 {
		t.Fatalf("owners = %d listed of %d roles, %d tables; want 20 of 21", len(g.Owners),
			g.OwnerRoles, g.TablesTotal)
	}
	lines := ownerLines(level(TrustGuide(GuideInput{Grants: g}), LevelAdvisory))
	if len(lines) != 20 || !strings.Contains(lines[19].Detail, "1 more owner role") {
		t.Fatalf("lines = %d, last %+v; want 20 and the one not listed", len(lines),
			lines[len(lines)-1])
	}
}

// Nil/empty: no user tables gives no owners and an unknown ownership grant.
func TestCheckGrantsNoUserTables(t *testing.T) {
	admin, ctx := freshInstall(t)
	g, err := CheckGrants(ctx, monitorPool(t, ctx, admin))
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	if g.Owners != nil || g.OwnerRoles != 0 || g.TablesTotal != 0 {
		t.Fatalf("empty database: owners %+v, %d roles, %d tables", g.Owners, g.OwnerRoles,
			g.TablesTotal)
	}
	l := level(TrustGuide(GuideInput{Grants: g}), LevelAdvisory)
	if own := grant(l, GrantTableOwnership); own.Name == "" || own.Present != nil ||
		len(ownerLines(l)) != 0 {
		t.Fatalf("ownership = %+v, owner lines %+v; want one unknown, none listed", own,
			ownerLines(l))
	}
}

// State transitions by version: pg_maintain leads on PostgreSQL 17+; before
// it ownership leads and pg_maintain is absent.
func TestCheckGrantsMaintainLeadsByVersion(t *testing.T) {
	admin, ctx := freshInstall(t)
	ownerRole(t, ctx, admin, uniq("own_v"), "vapp", 2)
	var version int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	g, err := CheckGrants(ctx, monitorPool(t, ctx, admin))
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	adv := level(TrustGuide(GuideInput{Grants: g}), LevelAdvisory)
	first := adv.Grants[0]
	if version >= 170000 {
		if first.Name != GrantMaintain || first.Present == nil || *first.Present {
			t.Fatalf("PG %d: first grant = %+v, want pg_maintain, missing", version, first)
		}
		return
	}
	if !strings.HasPrefix(first.Name, GrantTableOwnership) || g.Maintain != nil ||
		grant(adv, GrantMaintain).Name != "" {
		t.Fatalf("PG %d: first grant = %+v, maintain %v; want ownership, no pg_maintain",
			version, first, g.Maintain)
	}
}

// Integration: sequences the role cannot read in schemas a and b make the
// observation grant name exactly those; running its SQL makes it present.
func TestCheckGrantsSequenceSelect(t *testing.T) {
	admin, ctx := freshInstall(t)
	// The fixture's pg_hint_plan holds a sequence too (hint_plan.hints).
	execAll(t, ctx, admin, "DROP EXTENSION IF EXISTS pg_hint_plan")
	execAll(t, ctx, admin, "CREATE SCHEMA a", "CREATE SCHEMA b", "CREATE SCHEMA c",
		"CREATE TABLE a.t (id serial)", "CREATE SEQUENCE b.s", "CREATE SEQUENCE c.s")
	mon := monitorPool(t, ctx, admin)
	var role string
	if err := mon.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatalf("role: %v", err)
	}
	execAll(t, ctx, admin, "GRANT SELECT ON SEQUENCE c.s TO "+pgx.Identifier{role}.Sanitize())
	g, err := CheckGrants(ctx, mon)
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	obs := level(TrustGuide(GuideInput{Grants: g}), LevelObservation)
	sg := grant(obs, GrantSequenceSelect)
	want := "GRANT SELECT ON ALL SEQUENCES IN SCHEMA a, b TO " + quoteRole(role) + ";"
	if sg.Present == nil || *sg.Present || sg.SQL != want {
		t.Fatalf("sequence grant = %+v, want missing with %q", sg, want)
	}
	execAll(t, ctx, admin, sg.SQL)
	after, err := CheckGrants(ctx, mon)
	if err != nil {
		t.Fatalf("check after grant: %v", err)
	}
	sg = grant(level(TrustGuide(GuideInput{Grants: after}), LevelObservation),
		GrantSequenceSelect)
	if sg.Present == nil || !*sg.Present {
		t.Fatalf("after grant: sequence grant = %+v, want present", sg)
	}
}

// An extension's tables are not user tables: their owner is the
// extension's, which the operator does not choose, so they are neither
// counted nor listed (a superuser owner would get reassign advice that
// cannot apply).
func TestCheckGrantsIgnoresExtensionTables(t *testing.T) {
	admin, ctx := freshInstall(t)
	execAll(t, ctx, admin, "CREATE SCHEMA extonly", "CREATE TABLE extonly.t (id int)",
		"ALTER EXTENSION plpgsql ADD TABLE extonly.t")
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "ALTER EXTENSION plpgsql DROP TABLE extonly.t")
	})
	ownerRole(t, ctx, admin, uniq("own_x"), "xapp", 2)
	g, err := CheckGrants(ctx, monitorPool(t, ctx, admin))
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	if len(g.Owners) != 1 || g.Owners[0].Superuser || g.Owners[0].Tables != 2 ||
		g.OwnerRoles != 1 || g.TablesTotal != 2 {
		t.Fatalf("owners = %+v (%d roles, %d tables), want only the 2 user tables",
			g.Owners, g.OwnerRoles, g.TablesTotal)
	}
}
