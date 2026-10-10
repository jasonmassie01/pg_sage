package agentposture

import (
	"strings"
	"testing"
)

// AP-09: dblink callable by PUBLIC is critical; revoking EXECUTE clears it.
func TestAP09_DblinkExecutableByPublic(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE EXTENSION dblink SCHEMA " + f.schema)
	env := f.env(nil)
	o := f.run("AP-09", env)
	got := requireFinding(t, o, "dblink", Critical)
	if got.ObjectType != "extension" {
		t.Fatalf("AP-09 dblink object type = %q, want extension", got.ObjectType)
	}
	requireContains(t, "AP-09 fix", got.FixScript, "REVOKE EXECUTE ON FUNCTION "+
		f.schema+".dblink_connect(text) FROM PUBLIC")
	requireContains(t, "AP-09 detail", got.Detail, "PUBLIC")
	if strings.Contains(got.FixScript, "dblink_connect_u") {
		t.Fatalf("AP-09 fix revokes dblink_connect_u, which PUBLIC cannot run by default: %s",
			got.FixScript)
	}

	f.exec("REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA " + f.schema + " FROM PUBLIC")
	requireNoFinding(t, f.run("AP-09", env), "dblink")
}

// AP-09: dblink callable only by an exposed role that has no USAGE on the
// extension's schema is not reachable; with USAGE it is.
func TestAP09_DblinkNeedsSchemaUsage(t *testing.T) {
	f := newFixture(t)
	other := "pst_ext_" + suffix(t)
	f.exec("CREATE SCHEMA "+other, "CREATE EXTENSION dblink SCHEMA "+other,
		"REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA "+other+" FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION "+other+".dblink_connect(text) TO "+f.exposed)
	t.Cleanup(func() { _, _ = f.pool.Exec(f.ctx, "DROP SCHEMA IF EXISTS "+other+" CASCADE") })
	env := f.env(nil)
	requireNoFinding(t, f.run("AP-09", env), "dblink")

	f.exec("GRANT USAGE ON SCHEMA " + other + " TO " + f.exposed)
	got := requireFinding(t, f.run("AP-09", env), "dblink", Critical)
	requireContains(t, "AP-09 fix", got.FixScript, "FROM "+f.exposed)
	requireContains(t, "AP-09 detail", got.Detail, f.exposed)
}

// AP-09: postgres_fdw usable by exposed roles (wrapper USAGE, server USAGE)
// and a PUBLIC user mapping are each critical.
func TestAP09_PostgresFDWUsableByExposedRoles(t *testing.T) {
	f := newFixture(t)
	srv := "pst_srv_" + suffix(t)
	f.exec("CREATE EXTENSION postgres_fdw SCHEMA "+f.schema,
		"CREATE SERVER "+srv+" FOREIGN DATA WRAPPER postgres_fdw "+
			"OPTIONS (host '127.0.0.1', dbname 'postgres')")
	env := f.env(nil)
	o := f.run("AP-09", env)
	requireNoFinding(t, o, "postgres_fdw")
	requireNoFinding(t, o, srv)

	f.exec("GRANT USAGE ON FOREIGN DATA WRAPPER postgres_fdw TO PUBLIC",
		"GRANT USAGE ON FOREIGN SERVER "+srv+" TO "+f.exposed,
		"CREATE USER MAPPING FOR PUBLIC SERVER "+srv+" OPTIONS (user 'nobody')")
	o = f.run("AP-09", env)
	got := requireFinding(t, o, "postgres_fdw", Critical)
	requireContains(t, "AP-09 fdw fix", got.FixScript,
		"REVOKE USAGE ON FOREIGN DATA WRAPPER postgres_fdw FROM PUBLIC")
	got = requireFinding(t, o, srv, Critical)
	requireContains(t, "AP-09 server fix", got.FixScript,
		"REVOKE USAGE ON FOREIGN SERVER "+srv+" FROM "+f.exposed)
	got = requireFinding(t, o, "PUBLIC@"+srv, Critical)
	requireContains(t, "AP-09 mapping fix", got.FixScript,
		"DROP USER MAPPING FOR PUBLIC SERVER "+srv)
}

// AP-09: an untrusted language marked trusted (here: a language whose
// handler is PL/Perl's untrusted handler) is usable by PUBLIC: critical.
// A genuinely trusted language (plpgsql) is not reported.
func TestAP09_UntrustedLanguageMarkedTrusted(t *testing.T) {
	f := newFixture(t)
	lang := "pstlang_" + suffix(t)
	f.exec("CREATE FUNCTION "+f.q("plperlu_call_handler")+"() RETURNS language_handler "+
		"LANGUAGE c AS '$libdir/plpgsql', 'plpgsql_call_handler'",
		"CREATE TRUSTED LANGUAGE "+lang+" HANDLER "+f.q("plperlu_call_handler"))
	t.Cleanup(func() { _, _ = f.pool.Exec(f.ctx, "DROP LANGUAGE IF EXISTS "+lang+" CASCADE") })
	o := f.run("AP-09", f.env(nil))
	got := requireFinding(t, o, lang, Critical)
	if got.ObjectType != "language" {
		t.Fatalf("AP-09 language object type = %q", got.ObjectType)
	}
	requireContains(t, "AP-09 language fix", got.FixScript,
		"REVOKE USAGE ON LANGUAGE "+lang+" FROM PUBLIC")
	requireNoFinding(t, o, "plpgsql")

	f.exec("REVOKE USAGE ON LANGUAGE " + lang + " FROM PUBLIC")
	requireNoFinding(t, f.run("AP-09", f.env(nil)), lang)
}

// AP-09 on a database with none of it reports nothing.
func TestAP09_CleanDatabaseNoFindings(t *testing.T) {
	f := newFixture(t)
	o := f.run("AP-09", f.env(nil))
	for _, fd := range o.Findings {
		if strings.HasPrefix(fd.Object, "pst") {
			t.Fatalf("AP-09 reported a fixture object without a violation: %+v", fd)
		}
	}
}

// AP-15: PUBLIC grants on tables are reported per schema with a REVOKE per
// relation; grants to a named role and extension members are not.
func TestAP15_PublicTableGrants(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE TABLE "+f.q("pub_t")+" (id int)",
		"CREATE VIEW "+f.q("pub_v")+" AS SELECT 1 AS one",
		"CREATE TABLE "+f.q("named_t")+" (id int)",
		"GRANT SELECT, INSERT ON "+f.q("pub_t")+" TO PUBLIC",
		"GRANT SELECT ON "+f.q("pub_v")+" TO PUBLIC",
		"GRANT SELECT ON "+f.q("named_t")+" TO "+f.exposed)
	o := f.run("AP-15", f.env(nil))
	got := requireFinding(t, o, f.schema, Warning)
	if got.ObjectType != "schema" {
		t.Fatalf("AP-15 object type = %q, want schema", got.ObjectType)
	}
	requireContains(t, "AP-15 fix", got.FixScript,
		"REVOKE ALL ON TABLE "+f.q("pub_t")+" FROM PUBLIC;",
		"REVOKE ALL ON TABLE "+f.q("pub_v")+" FROM PUBLIC;")
	if strings.Contains(got.FixScript, "named_t") {
		t.Fatalf("AP-15 revokes a grant to a named role: %s", got.FixScript)
	}
	requireContains(t, "AP-15 detail", got.Detail, "2 ", "pub_t", "INSERT")
	if len(got.Evidence) != 2 {
		t.Fatalf("AP-15 evidence = %+v, want one per relation", got.Evidence)
	}
}

// AP-15 skips relations that belong to an extension (pg_stat_statements'
// view is granted to PUBLIC by the extension itself).
func TestAP15_ExtensionMembersAreNotReported(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE EXTENSION pg_stat_statements SCHEMA " + f.schema)
	requireNoFinding(t, f.run("AP-15", f.env(nil)), f.schema)
}

// AP-15: default privileges that grant PUBLIC on future tables, globally
// and per schema, are each reported with the ALTER DEFAULT PRIVILEGES fix.
func TestAP15_PublicDefaultPrivileges(t *testing.T) {
	f := newFixture(t)
	owner := f.role("pown_", "NOLOGIN")
	f.exec("ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" IN SCHEMA "+f.schema+
		" GRANT SELECT ON TABLES TO PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" GRANT USAGE ON SEQUENCES TO PUBLIC")
	o := f.run("AP-15", f.env(nil))
	got := requireFinding(t, o, owner+" in "+f.schema+": tables", Warning)
	if got.ObjectType != "default_privileges" {
		t.Fatalf("AP-15 default privileges object type = %q", got.ObjectType)
	}
	requireContains(t, "AP-15 default fix", got.FixScript, "ALTER DEFAULT PRIVILEGES FOR ROLE "+
		owner+" IN SCHEMA "+f.schema+" REVOKE ALL ON TABLES FROM PUBLIC;")
	got = requireFinding(t, o, owner+": sequences", Warning)
	requireContains(t, "AP-15 global default fix", got.FixScript,
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" REVOKE ALL ON SEQUENCES FROM PUBLIC;")

	f.exec("ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" IN SCHEMA "+f.schema+
		" REVOKE SELECT ON TABLES FROM PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+owner+" REVOKE USAGE ON SEQUENCES FROM PUBLIC")
	o = f.run("AP-15", f.env(nil))
	requireNoFinding(t, o, owner+" in "+f.schema+": tables")
	requireNoFinding(t, o, owner+": sequences")
}

// AP-15 with no PUBLIC grants in the fixture schema reports nothing there.
func TestAP15_NoPublicGrantsNoFinding(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE TABLE " + f.q("private_t") + " (id int)")
	requireNoFinding(t, f.run("AP-15", f.env(nil)), f.schema)
}
