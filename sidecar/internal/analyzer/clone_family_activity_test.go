package analyzer

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Phase 0 item 10: a schema family (tenant_000123 x N) is only called a
// leftover copy when it is idle: no scan or tuple activity since the
// stats reset (or none within the idle window), no statement or session
// referencing it. A live family is a "schema family" (e.g. multi-tenant):
// each issue is reported once, on one representative schema, with the
// list of affected schemas; nothing actionable is hidden and no drop is
// suggested.

var cloneNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func idleSignals() cloneSignals {
	return cloneSignals{sessionSchemas: map[string]bool{}, now: cloneNow,
		window: 7 * 24 * time.Hour, tracker: newCloneTracker()}
}

func tenantName(i int) string { return fmt.Sprintf("tenant_%06d", 120+i) }

// tenantSnapshot builds n tenant schemas with the same two tables.
func tenantSnapshot(n int) *collector.Snapshot {
	s := &collector.Snapshot{}
	for i := 1; i <= n; i++ {
		for _, tb := range []string{"orders", "customers"} {
			s.Tables = append(s.Tables, collector.TableStats{SchemaName: tenantName(i),
				RelName: tb})
		}
	}
	s.Tables = append(s.Tables, collector.TableStats{SchemaName: "public", RelName: "users"})
	return s
}

func tenantIssue(schema, severity string) Finding {
	return Finding{Category: "missing_fk_index", Severity: severity, ObjectType: "table",
		ObjectIdentifier: schema + ".orders", Title: "Missing FK index on " + schema + ".orders",
		Detail: map[string]any{"table": schema + ".orders"},
		RecommendedSQL: "CREATE INDEX CONCURRENTLY orders_customer_idx ON " + schema +
			".orders (customer_id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY " + schema + ".orders_customer_idx"}
}

func tenantFindings(n int) []Finding {
	var out []Finding
	for i := 1; i <= n; i++ {
		out = append(out, tenantIssue(tenantName(i), "warning"))
	}
	return out
}

func splitFamilyOutput(t *testing.T, out []Finding) (family []Finding, issues []Finding) {
	t.Helper()
	for _, f := range out {
		if f.Category == CategoryCloneSchemas {
			family = append(family, f)
		} else {
			issues = append(issues, f)
		}
	}
	return family, issues
}

func assertSchemaFamily(t *testing.T, out []Finding, members int) Finding {
	t.Helper()
	family, issues := splitFamilyOutput(t, out)
	if len(family) != 1 || len(issues) != 1 {
		t.Fatalf("family=%d issues=%d, want one family and one fanned-out issue: %+v",
			len(family), len(issues), out)
	}
	fam := family[0]
	if fam.Detail["family_kind"] != "schema_family" || fam.RecommendedSQL != "" ||
		strings.Contains(strings.ToLower(fam.Recommendation), "drop") ||
		strings.Contains(strings.ToLower(fam.Title), "leftover") {
		t.Fatalf("live family labeled as leftover or told to drop: %+v", fam)
	}
	issue := issues[0]
	affected, _ := issue.Detail["affected_schemas"].([]string)
	if len(affected) != members || issue.Detail["schema_family"] != fam.ObjectIdentifier {
		t.Fatalf("fan-out detail = %+v", issue.Detail)
	}
	if issue.RecommendedSQL == "" || !strings.Contains(issue.Title,
		fmt.Sprintf("%d more", members-1)) {
		t.Fatalf("fanned-out issue lost its action or count: %+v", issue)
	}
	return issue
}

func TestCloneFamily_LiveByScansIsFannedOut(t *testing.T) {
	snap := tenantSnapshot(6)
	snap.Tables[4].SeqScan = 12 // tenant_000123.orders is read
	out := collapseCloneSchemas(snap, tenantFindings(6), idleSignals())
	issue := assertSchemaFamily(t, out, 6)
	if issue.ObjectIdentifier != tenantName(1)+".orders" ||
		!strings.Contains(issue.RecommendedSQL, tenantName(1)+".orders") {
		t.Fatalf("representative = %+v, want the first schema's own finding", issue)
	}
	want := []string{tenantName(1), tenantName(2), tenantName(3), tenantName(4),
		tenantName(5), tenantName(6)}
	if !reflect.DeepEqual(issue.Detail["affected_schemas"], want) {
		t.Fatalf("affected = %v", issue.Detail["affected_schemas"])
	}
}

func TestCloneFamily_LiveByTupleWrites(t *testing.T) {
	for _, set := range []func(*collector.TableStats){
		func(ts *collector.TableStats) { ts.IdxScan = 1 },
		func(ts *collector.TableStats) { ts.NTupIns = 1 },
		func(ts *collector.TableStats) { ts.NTupUpd = 1 },
		func(ts *collector.TableStats) { ts.NTupDel = 1 },
	} {
		snap := tenantSnapshot(5)
		set(&snap.Tables[3])
		assertSchemaFamily(t, collapseCloneSchemas(snap, tenantFindings(5), idleSignals()), 5)
	}
}

func TestCloneFamily_LiveByStatementReference(t *testing.T) {
	snap := tenantSnapshot(5)
	snap.Queries = []collector.QueryStats{{QueryID: 1,
		Query: "SELECT * FROM tenant_000122.orders WHERE id = $1"}}
	assertSchemaFamily(t, collapseCloneSchemas(snap, tenantFindings(5), idleSignals()), 5)
	snap.Queries[0].Query = `SELECT * FROM "tenant_000122"."orders"`
	assertSchemaFamily(t, collapseCloneSchemas(snap, tenantFindings(5), idleSignals()), 5)
}

func TestCloneFamily_LiveBySessions(t *testing.T) {
	sig := idleSignals()
	sig.sessionSchemas = map[string]bool{tenantName(3): true}
	assertSchemaFamily(t, collapseCloneSchemas(tenantSnapshot(5), tenantFindings(5), sig), 5)
	sig = idleSignals()
	sig.sessionQueries = []string{"update tenant_000124.customers set x = 1"}
	assertSchemaFamily(t, collapseCloneSchemas(tenantSnapshot(5), tenantFindings(5), sig), 5)
	// Unknown sessions (the lookup failed) is not evidence of idleness.
	sig = idleSignals()
	sig.sessionSchemas = nil
	assertSchemaFamily(t, collapseCloneSchemas(tenantSnapshot(5), tenantFindings(5), sig), 5)
}

// Idle family: findings collapse into one leftover finding (lifeos-1).
func TestCloneFamily_IdleIsLeftover(t *testing.T) {
	out := collapseCloneSchemas(tenantSnapshot(5), tenantFindings(5), idleSignals())
	family, issues := splitFamilyOutput(t, out)
	if len(family) != 1 || len(issues) != 0 {
		t.Fatalf("idle family = %+v", out)
	}
	if family[0].Detail["family_kind"] != "leftover" || family[0].RecommendedSQL != "" {
		t.Fatalf("idle family finding = %+v", family[0])
	}
}

// Activity before the window: a family whose counters are non-zero but
// unchanged for the whole idle window becomes a leftover; any change
// makes it live again.
func TestCloneFamily_QuietWindow(t *testing.T) {
	snap := tenantSnapshot(5)
	snap.Tables[0].NTupIns = 400
	sig := idleSignals()
	kind := func() any {
		fam, _ := splitFamilyOutput(t, collapseCloneSchemas(snap, tenantFindings(5), sig))
		return fam[0].Detail["family_kind"]
	}
	if got := kind(); got != "schema_family" {
		t.Fatalf("first sight with activity = %v", got)
	}
	sig.now = cloneNow.Add(sig.window - time.Minute)
	if got := kind(); got != "schema_family" {
		t.Fatalf("quiet for less than the window = %v", got)
	}
	sig.now = cloneNow.Add(sig.window)
	if got := kind(); got != "leftover" {
		t.Fatalf("quiet for the window = %v, want leftover", got)
	}
	snap.Tables[0].NTupIns = 401
	sig.now = cloneNow.Add(sig.window + time.Hour)
	if got := kind(); got != "schema_family" {
		t.Fatalf("new activity = %v, want schema_family", got)
	}
}

// The representative of a fanned-out issue is its most severe instance.
func TestCloneFamily_RepresentativeIsMostSevere(t *testing.T) {
	snap := tenantSnapshot(5)
	snap.Tables[0].SeqScan = 1
	in := tenantFindings(5)
	in[3] = tenantIssue(tenantName(4), "critical")
	_, issues := splitFamilyOutput(t, collapseCloneSchemas(snap, in, idleSignals()))
	if len(issues) != 1 || issues[0].Severity != "critical" ||
		issues[0].ObjectIdentifier != tenantName(4)+".orders" {
		t.Fatalf("representative = %+v", issues)
	}
	// The input findings are not mutated.
	if _, ok := in[0].Detail["affected_schemas"]; ok {
		t.Fatal("input finding detail was mutated")
	}
}

// Distinct issues stay distinct; findings outside the family pass through.
func TestCloneFamily_DistinctIssuesAndOutsiders(t *testing.T) {
	snap := tenantSnapshot(5)
	snap.Tables[0].SeqScan = 1
	in := tenantFindings(5)
	for i := 1; i <= 2; i++ {
		f := tenantIssue(tenantName(i), "warning")
		f.Category, f.ObjectIdentifier = "unused_index", tenantName(i)+".orders_old_idx"
		in = append(in, f)
	}
	in = append(in, tenantIssue("public", "warning"),
		Finding{Category: "xid_wraparound", ObjectIdentifier: "database"})
	family, issues := splitFamilyOutput(t, collapseCloneSchemas(snap, in, idleSignals()))
	if len(family) != 1 || len(issues) != 4 {
		t.Fatalf("family=%d issues=%d (%+v), want 1 and 2 fanned-out + 2 outsiders",
			len(family), len(issues), issues)
	}
	if family[0].Detail["fanned_out_issues"] != 2 {
		t.Fatalf("family detail = %+v", family[0].Detail)
	}
}

func TestCloneTracker_Boundaries(t *testing.T) {
	tr := newCloneTracker()
	if d := tr.quietFor("k", 5, cloneNow); d != 0 {
		t.Fatalf("first sight quiet for %v", d)
	}
	if d := tr.quietFor("k", 5, cloneNow.Add(time.Hour)); d != time.Hour {
		t.Fatalf("unchanged = %v, want 1h", d)
	}
	if d := tr.quietFor("k", 6, cloneNow.Add(2*time.Hour)); d != 0 {
		t.Fatalf("changed = %v, want 0", d)
	}
	if d := tr.quietFor("other", 6, cloneNow.Add(2*time.Hour)); d != 0 {
		t.Fatalf("other key = %v", d)
	}
	var nilTracker *cloneTracker
	if d := nilTracker.quietFor("k", 1, cloneNow); d != 0 {
		t.Fatalf("nil tracker = %v", d)
	}
}

// Integration: a backend holding a lock on a tenant table and a running
// statement naming a schema are both visible to the session lookup.
func TestLoadCloneSessions_DB(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	for _, s := range []string{"DROP SCHEMA IF EXISTS tenant_990001 CASCADE",
		"CREATE SCHEMA tenant_990001", "CREATE TABLE tenant_990001.orders (id int)"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS tenant_990001 CASCADE")
	})
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE tenant_990001.orders IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	schemas, queries, err := a.loadCloneSessions(ctx)
	if err != nil {
		t.Fatalf("load sessions: %v", err)
	}
	if !schemas["tenant_990001"] {
		t.Fatalf("locked schema missing: %v", schemas)
	}
	found := false
	for _, q := range queries {
		found = found || strings.Contains(q, "tenant_990001.orders")
	}
	if !found {
		t.Fatalf("session statement text missing: %v", queries)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if s, _, err := a.loadCloneSessions(canceled); err == nil || s != nil {
		t.Fatalf("canceled lookup = %v %v, want an error and unknown sessions", s, err)
	}
}
