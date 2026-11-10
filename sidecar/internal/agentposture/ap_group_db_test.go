package agentposture

import (
	"strings"
	"testing"
)

// Grants to a group role an exposed role inherits reach the exposed role
// (agentgov-10). These tests use only the detectors' findings, so they
// also run against a build that ignores membership.

// groupFixture is a fixture whose exposed role inherits group role g.
func groupFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	g := f.role("pgrp_", "NOLOGIN")
	f.exec("GRANT " + g + " TO " + f.exposed)
	return f, g
}

func requireEvidence(t *testing.T, got Finding, source, detail string) {
	t.Helper()
	for _, e := range got.Evidence {
		if e.Source == source && strings.Contains(e.Detail, detail) {
			return
		}
	}
	t.Fatalf("%s evidence = %+v, want %s naming %q", got.Object, got.Evidence, source, detail)
}

// AP-03: a table granted to a group the exposed role inherits, RLS off.
func TestAP03_GroupGrantReachesMember(t *testing.T) {
	f, g := groupFixture(t)
	tbl := f.q("grp_t")
	f.exec("CREATE TABLE "+tbl+" (id int)", "GRANT SELECT ON "+tbl+" TO "+g)
	got := requireFinding(t, f.run("AP-03", f.env(nil)), tbl, Critical)
	path := f.exposed + " → " + g
	requireContains(t, "AP-03 group detail", got.Detail, path, "SELECT to "+g)
	requireContains(t, "AP-03 group fix", got.FixScript, "REVOKE ALL ON "+tbl+" FROM "+g+";",
		"REVOKE "+g+" FROM "+f.exposed+";")
	requireContains(t, "AP-03 group caveat", got.Caveat, "every member")
	requireEvidence(t, got, "pg_auth_members", path)
	if strings.Contains(got.FixScript, "REVOKE ALL ON "+tbl+" FROM "+f.exposed+";") {
		t.Fatalf("fix revokes from %s, which holds nothing directly:\n%s", f.exposed,
			got.FixScript)
	}
}

// AP-03: schema USAGE held only through the group still makes a table
// granted directly to the exposed role reachable.
func TestAP03_SchemaUsageThroughGroup(t *testing.T) {
	f, g := groupFixture(t)
	s := "pst_grpu_" + suffix(t)
	f.exec("CREATE SCHEMA "+s, "GRANT USAGE ON SCHEMA "+s+" TO "+g,
		"CREATE TABLE "+s+".t (id int)", "GRANT SELECT ON "+s+".t TO "+f.exposed)
	t.Cleanup(func() { dropSchema(f, s) })
	got := requireFinding(t, f.run("AP-03", f.env(nil)), s+".t", Critical)
	requireContains(t, "AP-03 usage evidence", got.Evidence[1].Detail, f.exposed)
	requireContains(t, "AP-03 usage fix", got.FixScript, "REVOKE ALL ON "+s+".t FROM "+
		f.exposed+";")
}

// A two-level chain anon → g1 → g2 reports the whole path, and the
// membership fix ends the exposed role's own membership.
func TestAP03_TwoLevelChainReportsFullPath(t *testing.T) {
	f, g1 := groupFixture(t)
	g2 := f.role("pgrq_", "NOLOGIN")
	tbl := f.q("chain_t")
	f.exec("GRANT "+g2+" TO "+g1, "CREATE TABLE "+tbl+" (id int)",
		"GRANT SELECT, INSERT ON "+tbl+" TO "+g2)
	got := requireFinding(t, f.run("AP-03", f.env(nil)), tbl, Critical)
	path := f.exposed + " → " + g1 + " → " + g2
	requireContains(t, "AP-03 chain detail", got.Detail, path)
	requireContains(t, "AP-03 chain fix", got.FixScript, "REVOKE ALL ON "+tbl+" FROM "+g2+";",
		"REVOKE "+g1+" FROM "+f.exposed+";")
	requireEvidence(t, got, "pg_auth_members", path)
}

// A member that does not inherit (it may only SET ROLE) holds nothing:
// PostgreSQL 14/15 by the member's NOINHERIT, 16+ by WITH INHERIT FALSE.
// mid inherits nothing from deep, so deep's grants stay unreported even
// though the exposed role inherits mid. On 16+ the per-grant option wins
// over the role's NOINHERIT attribute.
func TestAP03_NonInheritingMembershipIsNotFollowed(t *testing.T) {
	f := newFixture(t)
	g := f.role("pgrp_", "NOLOGIN")
	mid := f.role("pgrm_", "NOLOGIN")
	deep := f.role("pgrd_", "NOLOGIN")
	perGrant := f.env(nil).VersionNum >= 160000
	if perGrant {
		f.exec("GRANT "+g+" TO "+f.exposed+" WITH INHERIT FALSE",
			"GRANT "+mid+" TO "+f.exposed, "GRANT "+deep+" TO "+mid+" WITH INHERIT FALSE")
	} else {
		f.exec("ALTER ROLE "+mid+" NOINHERIT", "GRANT "+mid+" TO "+f.exposed,
			"GRANT "+deep+" TO "+mid, "GRANT "+g+" TO "+f.exposed)
	}
	for _, r := range []string{g, mid, deep} {
		f.exec("CREATE TABLE "+f.q("t_"+r)+" (id int)",
			"GRANT SELECT ON "+f.q("t_"+r)+" TO "+r)
	}
	o := f.run("AP-03", f.env(nil))
	requireFinding(t, o, f.q("t_"+mid), Critical)
	requireNoFinding(t, o, f.q("t_"+deep))
	if perGrant {
		requireNoFinding(t, o, f.q("t_"+g))
		f.exec("ALTER ROLE "+f.exposed+" NOINHERIT", "GRANT "+deep+" TO "+f.exposed+
			" WITH INHERIT TRUE")
		o = f.run("AP-03", f.env(nil))
		requireFinding(t, o, f.q("t_"+deep), Critical)
		requireFinding(t, o, f.q("t_"+mid), Critical)
		requireNoFinding(t, o, f.q("t_"+g))
		return
	}
	requireFinding(t, o, f.q("t_"+g), Critical)
	f.exec("ALTER ROLE " + f.exposed + " NOINHERIT")
	o = f.run("AP-03", f.env(nil))
	for _, r := range []string{g, mid, deep} {
		requireNoFinding(t, o, f.q("t_"+r))
	}
}

// AP-04: a permissive always-true policy TO the group applies to members.
func TestAP04_PolicyForGroupReachesMember(t *testing.T) {
	f, g := groupFixture(t)
	tbl := f.q("grp_docs")
	f.exec("CREATE TABLE "+tbl+" (id int)", "ALTER TABLE "+tbl+" ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY grp_all ON "+tbl+" FOR SELECT TO "+g+" USING (true)")
	got := requireFinding(t, f.run("AP-04", f.env(nil)), tbl+":grp_all", Warning)
	requireContains(t, "AP-04 group detail", got.Detail, f.exposed, f.exposed+" → "+g)
	requireEvidence(t, got, "pg_auth_members", f.exposed+" → "+g)
}

// AP-05: EXECUTE on a definer function granted to the group.
func TestAP05_DefinerFunctionGrantedToGroup(t *testing.T) {
	f, g := groupFixture(t)
	fn := f.q("grp_fn")
	f.exec("CREATE FUNCTION "+fn+"(a integer) RETURNS integer LANGUAGE sql "+
		"SECURITY DEFINER AS 'SELECT a'",
		"REVOKE EXECUTE ON FUNCTION "+fn+"(integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION "+fn+"(integer) TO "+g)
	got := requireFinding(t, f.run("AP-05", f.env(nil)), fn+"(a integer)", Warning)
	requireContains(t, "AP-05 group detail", got.Detail, f.exposed+" → "+g)
	requireContains(t, "AP-05 group fix", got.FixScript,
		"REVOKE EXECUTE ON FUNCTION "+fn+"(a integer) FROM "+g+";",
		"REVOKE "+g+" FROM "+f.exposed+";")
	requireContains(t, "AP-05 group caveat", got.Caveat, "every member")
}

// AP-06: a schema whose USAGE only the group holds is an exposed schema.
func TestAP06_SchemaUsageThroughGroup(t *testing.T) {
	f, g := groupFixture(t)
	s := "pst_grpv_" + suffix(t)
	f.exec("CREATE TABLE "+f.q("rls_t")+" (id int)",
		"ALTER TABLE "+f.q("rls_t")+" ENABLE ROW LEVEL SECURITY",
		"CREATE SCHEMA "+s, "GRANT USAGE ON SCHEMA "+s+" TO "+g,
		"CREATE VIEW "+s+".v AS SELECT id FROM "+f.q("rls_t"))
	t.Cleanup(func() { dropSchema(f, s) })
	got := requireFinding(t, f.run("AP-06", f.env(nil)), s+".v", Warning)
	requireContains(t, "AP-06 group detail", got.Detail, f.q("rls_t"))
}

// AP-09: server USAGE granted to the group reaches the member; a user
// mapping stays exact-match (mappings never inherit): one for the group
// itself is not the member's.
func TestAP09_ServerUsageThroughGroup(t *testing.T) {
	f, g := groupFixture(t)
	srv := "pst_srv_" + suffix(t)
	f.exec("CREATE EXTENSION postgres_fdw SCHEMA "+f.schema,
		"CREATE SERVER "+srv+" FOREIGN DATA WRAPPER postgres_fdw "+
			"OPTIONS (host '127.0.0.1', dbname 'postgres')",
		"GRANT USAGE ON FOREIGN SERVER "+srv+" TO "+g,
		"CREATE USER MAPPING FOR "+g+" SERVER "+srv+" OPTIONS (user 'nobody')",
		"CREATE USER MAPPING FOR "+f.exposed+" SERVER "+srv+" OPTIONS (user 'nobody')")
	o := f.run("AP-09", f.env(nil))
	got := requireFinding(t, o, srv, Critical)
	requireContains(t, "AP-09 group detail", got.Detail, f.exposed, f.exposed+" → "+g)
	requireContains(t, "AP-09 group fix", got.FixScript,
		"REVOKE USAGE ON FOREIGN SERVER "+srv+" FROM "+g+";")
	requireEvidence(t, got, "pg_auth_members", f.exposed+" → "+g)
	requireFinding(t, o, f.exposed+"@"+srv, Critical)
	requireNoFinding(t, o, g+"@"+srv)
}

// AP-06 on PostgreSQL 14 revokes the view; a grant to the group must go
// too, or the exposed role keeps reading it through the group. On 15+ the
// fix is security_invoker and names no role.
func TestAP06_FixClosesGroupGrant(t *testing.T) {
	f, g := groupFixture(t)
	s := "pst_grpw_" + suffix(t)
	f.exec("CREATE TABLE "+f.q("rls_w")+" (id int)",
		"ALTER TABLE "+f.q("rls_w")+" ENABLE ROW LEVEL SECURITY",
		"CREATE SCHEMA "+s, "GRANT USAGE ON SCHEMA "+s+" TO "+g,
		"CREATE VIEW "+s+".v AS SELECT id FROM "+f.q("rls_w"),
		"GRANT SELECT ON "+s+".v TO "+g)
	t.Cleanup(func() { dropSchema(f, s) })
	env := f.env(nil)
	got := requireFinding(t, f.run("AP-06", env), s+".v", Warning)
	if env.VersionNum >= 150000 {
		requireContains(t, "AP-06 15+ fix", got.FixScript, "security_invoker = true")
		return
	}
	requireContains(t, "AP-06 PG14 fix", got.FixScript, "REVOKE ALL ON "+s+".v FROM "+g+";")
	requireContains(t, "AP-06 PG14 caveat", got.Caveat, "every member")
	f.exec(got.FixScript)
	var can bool
	if err := f.pool.QueryRow(f.ctx, "SELECT has_table_privilege($1, $2, 'SELECT')",
		f.exposed, s+".v").Scan(&can); err != nil || can {
		t.Fatalf("after the fix %s can read %s.v: %v (err %v)", f.exposed, s, can, err)
	}
}
