//go:build cgo

package impact

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// tableChecksum is an md5 over every row of table in id order.
func tableChecksum(t *testing.T, e *env, table string) string {
	t.Helper()
	var sum string
	if err := e.f.Pool.QueryRow(e.ctx, "SELECT md5(string_agg(t::text, '|' ORDER BY t.id)) "+
		"FROM "+table+" t").Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

func tupUpdated(t *testing.T, e *env, table string) int64 {
	t.Helper()
	return e.f.Int64(`SELECT n_tup_upd FROM pg_stat_user_tables
		WHERE relid = $1::regclass`, table)
}

// CHECK-IMP-10: a backfill is sized from the plan-only EXPLAIN of its read
// shape and never runs.
func TestBackfillIsSizedFromItsReadShape(t *testing.T) {
	e := newEnv(t)
	before, updBefore := tableChecksum(t, e, "rv.rv_orders"), tupUpdated(t, e, "rv.rv_orders")
	a := e.analyze("UPDATE rv.rv_orders SET status = 'x' WHERE created_at < now()")
	f := one(t, a, RuleBackfill)
	p := decode[planData](t, citedOfKind(t, a, f, "plan")[0])
	truth := float64(e.f.Int64("SELECT count(*) FROM rv.rv_orders WHERE created_at < now()"))
	if p.Rows < truth/2 || p.Rows > truth*2 {
		t.Fatalf("estimated %v rows, want within 2x of %v", p.Rows, truth)
	}
	if !strings.HasPrefix(p.Shape, "SELECT 1 FROM rv.rv_orders") {
		t.Fatalf("read shape %q", p.Shape)
	}
	if !strings.Contains(f.Text, "single transaction") {
		t.Fatalf("CR-12 text %q does not note the single transaction", f.Text)
	}
	if after := tableChecksum(t, e, "rv.rv_orders"); after != before {
		t.Fatal("the review changed rv_orders' rows")
	}
	if err := flushStats(e); err != nil {
		t.Fatal(err)
	}
	if upd := tupUpdated(t, e, "rv.rv_orders"); upd != updBefore {
		t.Fatalf("n_tup_upd moved from %d to %d during the review", updBefore, upd)
	}
}

// CHECK-IMP-15: new objects, partitioned parents and PR statement plans.
func TestNewTableAntipatterns(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("CREATE TABLE rv.new_t (id int PRIMARY KEY, at timestamp)")
	fs := byRule(a, RuleNewObject)
	var intPK, tsNoTZ bool
	for _, f := range fs {
		if f.Severity != changereview.SeverityInfo {
			t.Errorf("CR-15 %q is %s, want info", f.Title, f.Severity)
		}
		intPK = intPK || strings.Contains(f.Title, "int4 primary key")
		tsNoTZ = tsNoTZ || strings.Contains(f.Title, "timestamp without time zone")
	}
	if !intPK || !tsNoTZ || len(fs) != 2 {
		t.Fatalf("want CR-15 info for the int4 key and the timestamp: %s", describe(a))
	}
	b := e.analyze("CREATE TABLE rv.no_pk (a bigint, b text)")
	if f := one(t, b, RuleNewObject); f.Severity != changereview.SeverityWarning ||
		!strings.Contains(f.Title, "no primary key") {
		t.Fatalf("CR-15 %+v, want a warning: no primary key", f)
	}
	c := e.analyze("CREATE TABLE rv.good (id bigint GENERATED ALWAYS AS IDENTITY " +
		"PRIMARY KEY, at timestamptz)")
	none(t, c, RuleNewObject)
}

func TestDDLOnAPartitionedParentWarns(t *testing.T) {
	e := newEnv(t)
	e.f.Exec(`CREATE TABLE rv.events (id bigint, at date NOT NULL) PARTITION BY RANGE (at)`)
	for _, y := range []string{"2024", "2025", "2026"} {
		e.f.Exec("CREATE TABLE rv.events_" + y + " PARTITION OF rv.events FOR VALUES FROM ('" +
			y + "-01-01') TO ('" + y + "-12-31')")
	}
	a := e.analyze("ALTER TABLE rv.events ADD COLUMN note text")
	f := one(t, a, RulePartitioned)
	if f.Severity != changereview.SeverityWarning || !strings.Contains(f.Text, "3 partitions") {
		t.Fatalf("CR-16 %+v, want a warning with 3 partitions", f)
	}
	tbl := decode[tableData](t, citedOfKind(t, a, f, "table")[0])
	if tbl.Partitions != 3 || tbl.Kind != "partitioned" {
		t.Fatalf("table evidence %+v", tbl)
	}
	none(t, e.analyze("ALTER TABLE rv.rv_items ADD COLUMN note text"), RulePartitioned)
}

func TestLargeSequentialScanInAPRStatement(t *testing.T) {
	e := newEnv(t)
	in := input("")
	in.SQLFiles = []changereview.SQLFile{{Path: "db/queries/items.sql",
		Content: "-- report\nSELECT id FROM rv.rv_items WHERE qty > $1"}}
	a := e.analyzeWith(e.eng, in)
	f := one(t, a, RuleStatementPlan)
	p := decode[planData](t, citedOfKind(t, a, f, "plan")[0])
	if f.Severity != changereview.SeverityWarning || !strings.Contains(f.Text, "Seq Scan") ||
		len(p.SeqScans) != 1 || p.SeqScans[0].Relation != "rv.rv_items" ||
		p.SeqScans[0].Rows < 100000 {
		t.Fatalf("CR-14 %+v plan %+v, want a warning naming the Seq Scan of rv_items", f, p)
	}
	if a.Statements[0].File != "db/queries/items.sql" || a.Statements[0].Line != 2 ||
		a.Statements[0].Kind != "select" || a.Statements[0].PlanSummary == "" {
		t.Fatalf("statement %+v", a.Statements[0])
	}
	b := e.analyze("SELECT email FROM rv.rv_orders WHERE id = $1")
	none(t, b, RuleStatementPlan)
}

// CHECK-IMP-11: published tables.
func TestPublishedTableChanges(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE PUBLICATION rv_pub FOR TABLE rv.rv_orders")
	a := e.analyze("ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address")
	if f := one(t, a, RulePublished); f.Severity != changereview.SeverityWarning ||
		!strings.Contains(f.Text, "rv_pub") {
		t.Fatalf("CR-13 %+v, want a warning naming rv_pub", f)
	}
	b := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN note text")
	if f := one(t, b, RulePublished); f.Severity != changereview.SeverityInfo {
		t.Fatalf("ADD COLUMN on a published table: CR-13 %s, want info", f.Severity)
	}
	none(t, e.analyze("ALTER TABLE rv.rv_items RENAME COLUMN qty TO quantity"), RulePublished)
}
