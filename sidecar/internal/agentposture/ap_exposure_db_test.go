package agentposture

import (
	"strings"
	"testing"
)

// AP-03: tables and views granted to exposed roles without row-level
// security.
func TestAP03_GrantedRelationsWithoutRLS(t *testing.T) {
	f := newFixture(t)
	other := f.role("poth_", "NOLOGIN")
	f.exec(
		"CREATE TABLE "+f.q("open_t")+" (id int, owner text)",
		"GRANT SELECT, UPDATE ON "+f.q("open_t")+" TO "+f.exposed,
		"CREATE TABLE "+f.q("rls_t")+" (id int, owner text)",
		"ALTER TABLE "+f.q("rls_t")+" ENABLE ROW LEVEL SECURITY",
		"GRANT SELECT ON "+f.q("rls_t")+" TO "+f.exposed,
		"CREATE TABLE "+f.q("other_t")+" (id int)",
		"GRANT SELECT ON "+f.q("other_t")+" TO "+other,
		"CREATE TABLE "+f.q("public_t")+" (id int)",
		"GRANT SELECT ON "+f.q("public_t")+" TO PUBLIC",
		"CREATE TABLE "+f.q("refs_t")+" (id int PRIMARY KEY)",
		"GRANT REFERENCES ON "+f.q("refs_t")+" TO "+f.exposed,
		"CREATE TABLE "+f.q("parted")+" (id int) PARTITION BY RANGE (id)",
		"GRANT INSERT ON "+f.q("parted")+" TO "+f.exposed,
		"CREATE VIEW "+f.q("open_v")+" AS SELECT id FROM "+f.q("open_t"),
		"GRANT SELECT ON "+f.q("open_v")+" TO "+f.exposed,
		"CREATE VIEW "+f.q("rls_v")+" AS SELECT id FROM "+f.q("rls_t"),
		"GRANT SELECT ON "+f.q("rls_v")+" TO "+f.exposed,
	)
	hidden := "pst_hidden_" + suffix(t)
	f.exec("CREATE SCHEMA "+hidden, "CREATE TABLE "+hidden+".t (id int)",
		"GRANT SELECT ON "+hidden+".t TO "+f.exposed)
	t.Cleanup(func() { dropSchema(f, hidden) })

	o := f.run("AP-03", f.env(nil))
	got := requireFinding(t, o, f.q("open_t"), Critical)
	requireContains(t, "AP-03 detail", got.Detail, f.exposed, "SELECT", "UPDATE")
	requireContains(t, "AP-03 fix", got.FixScript,
		"ALTER TABLE "+f.q("open_t")+" ENABLE ROW LEVEL SECURITY",
		"REVOKE ALL ON "+f.q("open_t")+" FROM "+f.exposed)
	if got.Caveat == "" || got.ObjectType != "table" {
		t.Fatalf("open_t finding = %+v, want a caveat and object type table", got)
	}
	got = requireFinding(t, o, f.q("public_t"), Critical)
	requireContains(t, "AP-03 detail", got.Detail, "PUBLIC")
	requireFinding(t, o, f.q("parted"), Critical)
	got = requireFinding(t, o, f.q("open_v"), Critical)
	requireContains(t, "AP-03 view detail", got.Detail, f.q("open_t"))
	if got.ObjectType != "view" {
		t.Fatalf("open_v object type = %s", got.ObjectType)
	}
	for _, obj := range []string{f.q("rls_t"), f.q("other_t"), f.q("refs_t"),
		f.q("rls_v"), hidden + ".t"} {
		requireNoFinding(t, o, obj)
	}
}

// A security_invoker view applies the caller's privileges and RLS, so it
// exposes nothing the base table does not (PostgreSQL 15+).
func TestAP03_SecurityInvokerViewIsNotExposure(t *testing.T) {
	f := newFixture(t)
	if f.env(nil).VersionNum < 150000 {
		t.Skip("recorded skip: security_invoker views need PostgreSQL 15+ " +
			"(PostgreSQL 14 views always run as their owner)")
	}
	f.exec("CREATE TABLE "+f.q("base")+" (id int)",
		"CREATE VIEW "+f.q("inv_v")+" WITH (security_invoker = true) AS SELECT id FROM "+
			f.q("base"),
		"GRANT SELECT ON "+f.q("inv_v")+" TO "+f.exposed)
	requireNoFinding(t, f.run("AP-03", f.env(nil)), f.q("inv_v"))
}

// AP-04: permissive policies for exposed roles that allow every row.
func TestAP04_PermissiveTruePolicies(t *testing.T) {
	f := newFixture(t)
	other := f.role("poth_", "NOLOGIN")
	tbl := f.q("docs")
	f.exec("CREATE TABLE "+tbl+" (id int, owner text)",
		"ALTER TABLE "+tbl+" ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY read_all ON "+tbl+" FOR SELECT TO "+f.exposed+" USING (true)",
		"CREATE POLICY insert_any ON "+tbl+" FOR INSERT TO PUBLIC WITH CHECK (true)",
		"CREATE POLICY strict_true ON "+tbl+" AS RESTRICTIVE FOR SELECT TO "+f.exposed+
			" USING (true)",
		"CREATE POLICY own_rows ON "+tbl+" FOR UPDATE TO "+f.exposed+
			" USING (owner = current_user)",
		"CREATE POLICY other_all ON "+tbl+" FOR DELETE TO "+other+" USING (true)")
	o := f.run("AP-04", f.env(nil))
	got := requireFinding(t, o, tbl+":read_all", Warning)
	requireContains(t, "AP-04 detail", got.Detail, f.exposed, "USING (true)")
	requireContains(t, "AP-04 fix", got.FixScript, "ALTER POLICY read_all ON "+tbl)
	if got.ObjectType != "policy" {
		t.Fatalf("object type = %s", got.ObjectType)
	}
	got = requireFinding(t, o, tbl+":insert_any", Warning)
	requireContains(t, "AP-04 detail", got.Detail, "PUBLIC", "WITH CHECK (true)")
	for _, p := range []string{"strict_true", "own_rows", "other_all"} {
		requireNoFinding(t, o, tbl+":"+p)
	}
}

// AP-05: SECURITY DEFINER functions exposed roles can execute without a
// pinned search_path.
func TestAP05_DefinerFunctionsWithoutSearchPath(t *testing.T) {
	f := newFixture(t)
	other := f.role("poth_", "NOLOGIN")
	def := func(name, extra string) string {
		return "CREATE FUNCTION " + f.q(name) + "(a integer) RETURNS integer " +
			"LANGUAGE sql SECURITY DEFINER " + extra + " AS 'SELECT a'"
	}
	f.exec(def("open_fn", ""),
		def("pinned_fn", "SET search_path = pg_catalog, pg_temp"),
		def("private_fn", ""),
		"REVOKE EXECUTE ON FUNCTION "+f.q("private_fn")+"(integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION "+f.q("private_fn")+"(integer) TO "+other,
		def("granted_fn", ""),
		"REVOKE EXECUTE ON FUNCTION "+f.q("granted_fn")+"(integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION "+f.q("granted_fn")+"(integer) TO "+f.exposed,
		"CREATE FUNCTION "+f.q("invoker_fn")+"() RETURNS int LANGUAGE sql AS 'SELECT 1'")
	hidden := "pst_hidden_" + suffix(t)
	f.exec("CREATE SCHEMA "+hidden, "CREATE FUNCTION "+hidden+
		".h() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'")
	t.Cleanup(func() { dropSchema(f, hidden) })

	o := f.run("AP-05", f.env(nil))
	got := requireFinding(t, o, f.q("open_fn")+"(a integer)", Warning)
	requireContains(t, "AP-05 detail", got.Detail, "PUBLIC")
	requireContains(t, "AP-05 fix", got.FixScript, "ALTER FUNCTION "+f.q("open_fn")+
		"(a integer) SET search_path = pg_catalog, pg_temp",
		"REVOKE EXECUTE ON FUNCTION "+f.q("open_fn")+"(a integer) FROM PUBLIC")
	got = requireFinding(t, o, f.q("granted_fn")+"(a integer)", Warning)
	requireContains(t, "AP-05 detail", got.Detail, f.exposed)
	for _, obj := range []string{f.q("pinned_fn") + "(a integer)",
		f.q("private_fn") + "(a integer)", f.q("invoker_fn") + "()", hidden + ".h()"} {
		requireNoFinding(t, o, obj)
	}
}

// AP-06: views in exposed schemas over RLS tables that run as their owner.
// PostgreSQL 14 has no security_invoker: there it is reported as such.
func TestAP06_ViewsOverRLSTablesWithoutSecurityInvoker(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE TABLE "+f.q("rls_t")+" (id int)",
		"ALTER TABLE "+f.q("rls_t")+" ENABLE ROW LEVEL SECURITY",
		"CREATE TABLE "+f.q("plain_t")+" (id int)",
		"CREATE VIEW "+f.q("owner_v")+" AS SELECT id FROM "+f.q("rls_t"),
		"CREATE VIEW "+f.q("plain_v")+" AS SELECT id FROM "+f.q("plain_t"))
	hidden := "pst_hidden_" + suffix(t)
	f.exec("CREATE SCHEMA "+hidden, "CREATE VIEW "+hidden+".v AS SELECT id FROM "+
		f.q("rls_t"))
	t.Cleanup(func() { dropSchema(f, hidden) })
	env := f.env(nil)
	o := f.run("AP-06", env)
	got := requireFinding(t, o, f.q("owner_v"), Warning)
	requireContains(t, "AP-06 detail", got.Detail, f.q("rls_t"))
	requireNoFinding(t, o, f.q("plain_v"))
	requireNoFinding(t, o, hidden+".v")
	if env.VersionNum < 150000 {
		requireContains(t, "AP-06 PG14 title", got.Title, "no security_invoker available")
		requireContains(t, "AP-06 PG14 fix", got.FixScript, "REVOKE")
		requireSkipped(t, o, "security_invoker")
		return
	}
	requireContains(t, "AP-06 fix", got.FixScript, "ALTER VIEW "+f.q("owner_v")+
		" SET (security_invoker = true)")
	requireSkipped(t, o, "no_security_invoker")
	f.exec("ALTER VIEW " + f.q("owner_v") + " SET (security_invoker = true)")
	requireNoFinding(t, f.run("AP-06", env), f.q("owner_v"))
}

func requireSkipped(t *testing.T, o Outcome, arm string) {
	t.Helper()
	for _, s := range o.Skipped {
		if s.Arm == arm && s.Reason != "" {
			return
		}
	}
	t.Fatalf("%s skipped %+v, want arm %s with a reason", o.Detector, o.Skipped, arm)
}

// AP-07: PUBLIC may create objects in a schema.
func TestAP07_PublicCreateOnSchemas(t *testing.T) {
	f := newFixture(t)
	other := f.role("poth_", "NOLOGIN")
	open := "pst_open_" + suffix(t)
	granted := "pst_granted_" + suffix(t)
	f.exec("CREATE SCHEMA "+open, "GRANT CREATE ON SCHEMA "+open+" TO PUBLIC",
		"CREATE SCHEMA "+granted, "GRANT CREATE ON SCHEMA "+granted+" TO "+other)
	t.Cleanup(func() { dropSchema(f, open); dropSchema(f, granted) })
	o := f.run("AP-07", f.env(nil))
	got := requireFinding(t, o, open, Warning)
	requireContains(t, "AP-07 fix", got.FixScript, "REVOKE CREATE ON SCHEMA "+open+
		" FROM PUBLIC")
	if got.ObjectType != "schema" {
		t.Fatalf("object type = %s", got.ObjectType)
	}
	requireNoFinding(t, o, granted)
	requireNoFinding(t, o, f.schema)
	for _, fd := range o.Findings {
		if strings.HasPrefix(fd.Object, "pg_") || fd.Object == "information_schema" {
			t.Fatalf("system schema reported: %+v", fd)
		}
	}
}

func dropSchema(f *fixture, name string) {
	_, _ = f.pool.Exec(f.ctx, "DROP SCHEMA IF EXISTS "+name+" CASCADE")
}

// Objects an extension owns are not the operator's grants: pg_hint_plan's
// hint_plan.hints is readable by PUBLIC by design, and enabling RLS on an
// extension's table is not a fix anyone can apply. AP-03 and AP-06 skip
// extension members.
func TestAP03_SkipsExtensionMembers(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(f.ctx, "CREATE EXTENSION IF NOT EXISTS pg_hint_plan"); err != nil {
		t.Skipf("recorded skip: pg_hint_plan is not installed on this server: %v", err)
	}
	var public bool
	if err := f.pool.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c
		CROSS JOIN LATERAL aclexplode(c.relacl) a
		WHERE c.oid = 'hint_plan.hints'::regclass AND a.grantee = 0)`).Scan(&public); err != nil {
		t.Fatal(err)
	}
	if !public {
		t.Fatal("hint_plan.hints is not granted to PUBLIC: the control case is gone")
	}
	requireNoFinding(t, f.run("AP-03", f.env(nil)), "hint_plan.hints")
}
