//go:build cgo

package impact

import (
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// CHECK-IMP-02: a renamed column that a hot statement reads is critical
// and cites exactly that statement.
func TestRenameColumnCitesTheStatementsReadingIt(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address")
	f := one(t, a, RuleColumnReferenced)
	if f.Severity != changereview.SeverityCritical {
		t.Fatalf("CR-03 severity %s, want critical", f.Severity)
	}
	if ids := queryIDsOf(t, a, f); !slices.Equal(ids, []int64{e.f.Q1.ID}) {
		t.Fatalf("CR-03 cites queryids %v, want exactly Q1 %d", ids, e.f.Q1.ID)
	}
	q := decode[queryData](t, evidence(t, a, queryEvidenceID(e.f.Q1.ID)))
	if q.Calls < 2000 || !slices.Contains(q.Columns, "email") {
		t.Fatalf("Q1 evidence: calls %d columns %v, want >= 2000 reading email",
			q.Calls, q.Columns)
	}
	if q.Tags["route"] != "GET /orders/:id" || q.Tags["application"] != "billing-api" {
		t.Fatalf("Q1 sqlcommenter tags %v", q.Tags)
	}
	if !near(q.PeakPerS, e.f.Q1.RatePerSecond(), 0.01) || q.Calls7d != e.f.Q1.Calls {
		t.Fatalf("Q1 rate %v/s and 7-day calls %d, want %v/s and %d", q.PeakPerS,
			q.Calls7d, e.f.Q1.RatePerSecond(), e.f.Q1.Calls)
	}
	if verdict(a) != changereview.VerdictRisky {
		t.Fatalf("verdict %s, want risky", verdict(a))
	}
}

func TestDropColumnCitesTheWriteThatSetsIt(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_orders DROP COLUMN status")
	f := one(t, a, RuleColumnReferenced)
	if f.Severity != changereview.SeverityCritical {
		t.Fatalf("CR-03 severity %s, want critical", f.Severity)
	}
	if ids := queryIDsOf(t, a, f); !slices.Equal(ids, []int64{e.f.Q2.ID}) {
		t.Fatalf("CR-03 cites %v, want exactly Q2 %d", ids, e.f.Q2.ID)
	}
	if !strings.Contains(f.Text, "500") {
		t.Fatalf("CR-03 text %q does not carry Q2's 500 calls", f.Text)
	}
}

func TestAddNullableColumnIsSafe(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN note text")
	none(t, a, RuleColumnReferenced, RuleRelationReferenced, RuleWillFail,
		RuleTypeChange, RuleIndexUsed, RuleImpactUnknown)
	if verdict(a) != changereview.VerdictSafe || a.ParseMode != changereview.ParseAST {
		t.Fatalf("verdict %s (%s), want safe:%s", verdict(a), a.ParseMode, describe(a))
	}
	if len(a.Statements) != 1 || a.Statements[0].Kind != "add_column" ||
		a.Statements[0].EvidenceID != "stmt:1" || a.Statements[0].Fingerprint == "" {
		t.Fatalf("statements %+v", a.Statements)
	}
	if !slices.Equal(a.Expectations.Relations, []string{"rv.rv_orders"}) {
		t.Fatalf("expectation relations %v", a.Expectations.Relations)
	}
}

func TestFunctionBodyMentioningTheColumnWarns(t *testing.T) {
	e := newEnv(t)
	e.f.Exec(`CREATE FUNCTION rv.order_email(oid bigint) RETURNS text LANGUAGE plpgsql
		AS $$ BEGIN RETURN (SELECT email FROM rv.rv_orders WHERE id = oid); END $$`)
	e.f.Exec(`CREATE FUNCTION rv.unrelated() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
	a := e.analyze("ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address")
	fs := byRule(a, RuleColumnReferenced)
	var warn *changereview.Finding
	for i := range fs {
		if fs[i].Severity == changereview.SeverityWarning {
			warn = &fs[i]
		}
	}
	if len(fs) != 2 || warn == nil {
		t.Fatalf("want a critical and a warning CR-03, got %s", describe(a))
	}
	if !strings.Contains(warn.Title+warn.Text, "rv.order_email") ||
		strings.Contains(warn.Title+warn.Text, "unrelated") {
		t.Fatalf("CR-03 warning %q / %q must name rv.order_email only", warn.Title,
			warn.Text)
	}
	fn := citedOfKind(t, a, *warn, "function")
	if len(fn) != 1 || decode[functionData](t, fn[0]).Function != "rv.order_email" {
		t.Fatalf("CR-03 warning cites functions %v", fn)
	}
}

// CHECK-IMP-03: changes PostgreSQL refuses or that cannot succeed.
func TestTypeChangeUnderAViewWillFail(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE VIEW rv.order_status AS SELECT id, status FROM rv.rv_orders")
	a := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN status TYPE varchar(20)")
	f := one(t, a, RuleWillFail)
	if f.Severity != changereview.SeverityCritical ||
		!strings.Contains(f.Text, "rv.order_status") {
		t.Fatalf("CR-05 %+v, want critical naming rv.order_status", f)
	}
	v := citedOfKind(t, a, f, "view")
	if len(v) != 1 || !slices.Contains(decode[viewData](t, v[0]).Columns, "status") {
		t.Fatalf("CR-05 view evidence %v", v)
	}
	b := e.analyze("ALTER TABLE rv.rv_orders DROP COLUMN status")
	if f := one(t, b, RuleWillFail); f.Severity != changereview.SeverityCritical {
		t.Fatalf("DROP COLUMN under a view: CR-05 %s, want critical", f.Severity)
	}
	c := e.analyze("ALTER TABLE rv.rv_orders DROP COLUMN email")
	none(t, c, RuleWillFail)
}

func TestAddNotNullColumnWithoutDefaultWillFail(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN flag boolean NOT NULL")
	f := one(t, a, RuleWillFail)
	if f.Severity != changereview.SeverityCritical {
		t.Fatalf("CR-05 severity %s, want critical", f.Severity)
	}
	b := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN flag boolean NOT NULL DEFAULT false")
	none(t, b, RuleWillFail)
	c := e.analyze(`CREATE TABLE rv.empty_t (id bigint PRIMARY KEY);
		ALTER TABLE rv.empty_t ADD COLUMN flag boolean NOT NULL`)
	none(t, c, RuleWillFail)
}

// CHECK-IMP-08: SET NOT NULL over a column with NULLs will fail; without
// the statistics it is unknown, never safe.
func TestSetNotNullOverNullsWillFail(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("UPDATE rv.rv_orders SET customer_id = NULL WHERE id % 33 = 0")
	e.f.Exec("ANALYZE rv.rv_orders")
	var nullFrac, reltuples float64
	if err := e.f.Pool.QueryRow(e.ctx, `SELECT s.null_frac, c.reltuples FROM pg_stats s,
		pg_class c WHERE s.schemaname = 'rv' AND s.tablename = 'rv_orders'
		AND s.attname = 'customer_id' AND c.oid = 'rv.rv_orders'::regclass`).
		Scan(&nullFrac, &reltuples); err != nil {
		t.Fatal(err)
	}
	a := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN customer_id SET NOT NULL")
	f := one(t, a, RuleWillFail)
	if f.Severity != changereview.SeverityCritical || !strings.Contains(f.Title, "will fail") {
		t.Fatalf("CR-05 %+v, want critical 'will fail'", f)
	}
	n := citedOfKind(t, a, f, "nulls")
	if len(n) != 1 {
		t.Fatalf("CR-05 cites null evidence %v", n)
	}
	got := decode[nullsData](t, n[0])
	if !near(got.EstimatedNullRows, nullFrac*reltuples, 0.01) || got.NullFrac <= 0 {
		t.Fatalf("estimated NULL rows %v, want %v within 1%%", got.EstimatedNullRows,
			nullFrac*reltuples)
	}
	none(t, a, RuleImpactUnknown)
}

func TestSetNotNullWithoutSelectPrivilegeIsUnknown(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.f.ReaderPool(rvReaderWithout("rv_orders"))
	a := e.analyzeWith(e.engine(pool, nil),
		input("ALTER TABLE rv.rv_orders ALTER COLUMN customer_id SET NOT NULL"))
	var found bool
	for _, f := range byRule(a, RuleImpactUnknown) {
		if f.Severity == changereview.SeverityWarning &&
			strings.Contains(f.Title+f.Text, "null fraction unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want CR-17 warning 'null fraction unavailable': %s", describe(a))
	}
	if verdict(a) == changereview.VerdictSafe {
		t.Fatal("an unknown null fraction must never be safe")
	}
}
