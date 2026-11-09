package agentposture

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Large catalogs: capped reads report their truncation, and schemas with
// more than groupThreshold findings get one schema finding.

// withLimit sets a row cap for one test (detector tests run serially).
func withLimit(t *testing.T, cap *int, n int) {
	t.Helper()
	old := *cap
	*cap = n
	t.Cleanup(func() { *cap = old })
}

// tables creates tables t<from>..t<to> in the fixture schema, granted to
// grantee.
func (f *fixture) tables(from, to int, grantee string) {
	f.t.Helper()
	f.exec(fmt.Sprintf(`DO $$ DECLARE t text; BEGIN FOR i IN %d..%d LOOP
  t := 't' || lpad(i::text, 2, '0');
  EXECUTE pg_catalog.format('CREATE TABLE %%I.%%I (id int)', '%s', t);
  EXECUTE pg_catalog.format('GRANT SELECT ON %%I.%%I TO %s', '%s', t);
END LOOP; END $$`, from, to, f.schema, grantee, f.schema))
}

func objectsIn(o Outcome, schema string) []string {
	var out []string
	for _, fd := range o.Findings {
		if fd.Object == schema || strings.HasPrefix(fd.Object, schema+".") {
			out = append(out, fd.Object)
		}
	}
	return out
}

func evaluated(o Outcome) bool {
	return slices.Contains(Result{Outcomes: []Outcome{o}}.EvaluatedCategories(),
		CategoryPrefix+o.Detector)
}

// Boundary: four exposed tables over a limit of three are truncated and
// AP-03 is not evaluated; exactly three are not truncated.
func TestAP03_TruncatesPastTheRowLimit(t *testing.T) {
	f := newFixture(t)
	withLimit(t, &maxRows, 3)
	f.tables(1, 3, f.exposed)
	o := f.run("AP-03", f.env(nil))
	if len(o.Truncated) != 0 || !evaluated(o) {
		t.Fatalf("3 rows at limit 3: truncated %v, evaluated %v", o.Truncated, evaluated(o))
	}
	if got := objectsIn(o, f.schema); len(got) != 3 {
		t.Fatalf("3 rows at limit 3: findings %v", got)
	}
	f.tables(4, 4, f.exposed)
	o = f.run("AP-03", f.env(nil))
	if !slices.Equal(o.Truncated, []Truncation{{Arm: "relations", Limit: 3}}) {
		t.Fatalf("4 rows at limit 3: truncated = %v, want relations at 3", o.Truncated)
	}
	if evaluated(o) {
		t.Fatal("a truncated AP-03 was evaluated: findings past the cut would resolve")
	}
	want := []string{f.q("t01"), f.q("t02"), f.q("t03")}
	if got := objectsIn(o, f.schema); !slices.Equal(got, want) {
		t.Fatalf("findings = %v, want the first three %v", got, want)
	}
	requireContains(t, "AP-03 note", o.Note(), "relations: first 3 rows read")
}

// Nil/empty: no rows give no truncation and no findings, even at limit 0.
func TestAP03_ZeroRowsAreNotTruncated(t *testing.T) {
	f := newFixture(t)
	for _, limit := range []int{3, 0} {
		withLimit(t, &maxRows, limit)
		o := f.run("AP-03", f.env(nil))
		if len(o.Truncated) != 0 || len(objectsIn(o, f.schema)) != 0 || !evaluated(o) {
			t.Fatalf("limit %d, empty schema: %+v", limit, o)
		}
	}
}

// AP-09..AP-16 reads have their own cap: AP-15 at 1 with two PUBLIC
// grants is truncated, with one it is not.
func TestAP15_TruncatesPastTheRowLimit(t *testing.T) {
	f := newFixture(t)
	withLimit(t, &apbMaxRows, 1)
	f.tables(1, 1, "PUBLIC")
	if o := f.run("AP-15", f.env(nil)); len(o.Truncated) != 0 || !evaluated(o) {
		t.Fatalf("1 row at limit 1: truncated %v", o.Truncated)
	}
	f.tables(2, 2, "PUBLIC")
	o := f.run("AP-15", f.env(nil))
	if !slices.Equal(o.Truncated, []Truncation{{Arm: "PUBLIC grants", Limit: 1}}) ||
		evaluated(o) {
		t.Fatalf("2 rows at limit 1: truncated %v, evaluated %v", o.Truncated, evaluated(o))
	}
}

// Happy path: 20 exposed tables give 20 findings, 21 give one schema
// finding, 25 one listing all 25; its fix script clears them.
func TestAP03_GroupsLargeSchemas(t *testing.T) {
	f := newFixture(t)
	f.tables(1, 20, f.exposed)
	o := f.run("AP-03", f.env(nil))
	if got := objectsIn(o, f.schema); len(got) != 20 || got[0] != f.q("t01") {
		t.Fatalf("20 tables: findings %v, want 20 per-table", got)
	}
	f.tables(21, 21, f.exposed)
	o = f.run("AP-03", f.env(nil))
	if got := objectsIn(o, f.schema); !slices.Equal(got, []string{f.schema}) {
		t.Fatalf("21 tables: findings %v, want one on schema %s", got, f.schema)
	}
	f.tables(22, 24, f.exposed)
	f.tables(25, 25, "PUBLIC")
	o = f.run("AP-03", f.env(nil))
	got := requireFinding(t, o, f.schema, Critical)
	if got.ObjectType != "schema" || len(objectsIn(o, f.schema)) != 1 {
		t.Fatalf("25 tables: %+v, want one schema finding", got)
	}
	if len(got.Evidence) != 26 || got.Evidence[0].Detail != "25 tables and views" {
		t.Fatalf("evidence = %+v, want the count and 25 tables", got.Evidence)
	}
	for i := 1; i <= 25; i++ {
		ref := f.q(fmt.Sprintf("t%02d", i))
		if !slices.ContainsFunc(got.Evidence, func(e Evidence) bool { return e.Ref == ref }) {
			t.Fatalf("evidence does not list %s", ref)
		}
	}
	requireContains(t, "AP-03 schema title", got.Title, "25 tables and views", f.schema)
	requireContains(t, "AP-03 schema detail", got.Detail, f.exposed)
	requireContains(t, "AP-03 schema fix", got.FixScript,
		"REVOKE ALL ON ALL TABLES IN SCHEMA "+f.schema+" FROM "+f.exposed+";",
		"REVOKE ALL ON ALL TABLES IN SCHEMA "+f.schema+" FROM PUBLIC;",
		"-- ALTER TABLE "+f.q("t01")+" ENABLE ROW LEVEL SECURITY;")
	if got.Caveat == "" {
		t.Fatal("the schema-wide revoke has no caveat")
	}
	f.exec(got.FixScript)
	if left := objectsIn(f.run("AP-03", f.env(nil)), f.schema); len(left) != 0 {
		t.Fatalf("after the fix script: %v", left)
	}
}

// AP-05 groups definer functions; the schema fix pins every one of them.
func TestAP05_GroupsLargeSchemas(t *testing.T) {
	f := newFixture(t)
	f.exec(fmt.Sprintf(`DO $$ BEGIN FOR i IN 1..21 LOOP
  EXECUTE pg_catalog.format('CREATE FUNCTION %%I.%%I(a integer) RETURNS integer '
    'LANGUAGE sql SECURITY DEFINER AS ''SELECT a''', '%s', 'f' || lpad(i::text, 2, '0'));
END LOOP; END $$`, f.schema),
		"CREATE PROCEDURE "+f.q("p22")+"() LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'")
	o := f.run("AP-05", f.env(nil))
	got := requireFinding(t, o, f.schema, Warning)
	if got.ObjectType != "schema" || len(objectsIn(o, f.schema)) != 1 {
		t.Fatalf("22 routines: %v, want one schema finding", objectsIn(o, f.schema))
	}
	if got.Evidence[0].Detail != "22 definer functions" ||
		got.Evidence[1].Ref != f.q("f01")+"(a integer)" {
		t.Fatalf("evidence = %+v", got.Evidence[:2])
	}
	requireContains(t, "AP-05 schema fix", got.FixScript, "DO $fix$",
		"SET search_path = pg_catalog, pg_temp",
		"-- REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA "+f.schema+" FROM PUBLIC;")
	f.exec(got.FixScript)
	if left := objectsIn(f.run("AP-05", f.env(nil)), f.schema); len(left) != 0 {
		t.Fatalf("after the fix script: %v", left)
	}
}

// AP-06 groups owner-rights views; on PostgreSQL 15+ the schema fix makes
// each one a security_invoker view.
func TestAP06_GroupsLargeSchemas(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE TABLE "+f.q("rls_t")+" (id int)",
		"ALTER TABLE "+f.q("rls_t")+" ENABLE ROW LEVEL SECURITY",
		fmt.Sprintf(`DO $$ BEGIN FOR i IN 1..21 LOOP
  EXECUTE pg_catalog.format('CREATE VIEW %%I.%%I AS SELECT id FROM %%I.rls_t', '%s',
    'v' || lpad(i::text, 2, '0'), '%s');
END LOOP; END $$`, f.schema, f.schema))
	env := f.env(nil)
	o := f.run("AP-06", env)
	got := requireFinding(t, o, f.schema, Warning)
	if got.ObjectType != "schema" || len(objectsIn(o, f.schema)) != 1 {
		t.Fatalf("21 views: %v, want one schema finding", objectsIn(o, f.schema))
	}
	requireContains(t, "AP-06 schema detail", got.Detail, "21 views", f.q("rls_t"))
	f.exec(got.FixScript)
	if env.VersionNum < 150000 {
		requireContains(t, "AP-06 PG14 fix", got.FixScript, "REVOKE ALL ON %s FROM PUBLIC")
		return
	}
	requireContains(t, "AP-06 fix", got.FixScript, "SET (security_invoker = true)")
	if left := objectsIn(f.run("AP-06", env), f.schema); len(left) != 0 {
		t.Fatalf("after the fix script: %v", left)
	}
}
