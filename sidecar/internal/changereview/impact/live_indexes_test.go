//go:build cgo

package impact

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// indexOf is the index evidence a CR-07 finding cites.
func indexOf(t *testing.T, a changereview.Analysis, f changereview.Finding) indexData {
	t.Helper()
	ev := citedOfKind(t, a, f, "index")
	if len(ev) != 1 {
		t.Fatalf("CR-07 cites %d index evidence, want 1", len(ev))
	}
	return decode[indexData](t, ev[0])
}

// CHECK-IMP-05: dropping an index that is used, planned or required.
func TestDropScannedIndexIsCritical(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("DROP INDEX rv.rv_orders_created_idx")
	f := one(t, a, RuleIndexUsed)
	ix := indexOf(t, a, f)
	if f.Severity != changereview.SeverityCritical || ix.Scans <= 0 {
		t.Fatalf("CR-07 %s with %d scans, want critical with scans > 0", f.Severity, ix.Scans)
	}
	if ix.Relation != "rv.rv_orders" || ix.Index != "rv.rv_orders_created_idx" {
		t.Fatalf("index evidence %+v", ix)
	}
}

func TestPlanUsersFromTheExplainCache(t *testing.T) {
	e := newEnv(t)
	e.f.Exec(`INSERT INTO sage.explain_cache (queryid, query_text, plan_json, source)
		VALUES ($1, $2, '[{"Plan": {"Node Type": "Aggregate", "Plans": [{"Node Type":
		"Index Only Scan", "Index Name": "rv_orders_created_idx"}]}}]', 'test')`,
		e.f.Q3.ID, e.f.Q3.Text)
	e.f.Exec(`INSERT INTO sage.explain_cache (queryid, query_text, plan_json, source)
		VALUES ($1, $2, '[{"Plan": {"Node Type": "Index Scan",
		"Index Name": "rv_orders_pkey"}}]', 'test')`, e.f.Q1.ID, e.f.Q1.Text)
	a := e.analyze("DROP INDEX rv.rv_orders_created_idx")
	ix := indexOf(t, a, one(t, a, RuleIndexUsed))
	if !slices.Equal(ix.PlanUsers, []int64{e.f.Q3.ID}) {
		t.Fatalf("plan users %v, want only Q3 %d", ix.PlanUsers, e.f.Q3.ID)
	}
}

func TestLastIndexScanIsReportedOnPG16(t *testing.T) {
	e := newEnv(t)
	testdb.RequireServerVersion(t, e.f.Pool, 160000, "pg_stat_all_indexes.last_idx_scan")
	a := e.analyze("DROP INDEX rv.rv_orders_created_idx")
	ix := indexOf(t, a, one(t, a, RuleIndexUsed))
	if ix.LastScan == nil || ix.LastScan.IsZero() {
		t.Fatalf("last_idx_scan not reported on PG16+: %+v", ix)
	}
}

func TestDropConstraintBackedIndexIsCritical(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("ALTER TABLE rv.rv_orders ADD CONSTRAINT rv_orders_email_key UNIQUE (email)")
	a := e.analyze("DROP INDEX rv.rv_orders_email_key")
	f := one(t, a, RuleIndexUsed)
	ix := indexOf(t, a, f)
	backed := []string{"rv_orders_email_key"}
	if f.Severity != changereview.SeverityCritical || !strings.Contains(f.Title,
		"constraint-backed") || !slices.Equal(ix.ConstraintBacked, backed) {
		t.Fatalf("CR-07 %+v backed by %v, want critical constraint-backed", f,
			ix.ConstraintBacked)
	}
	if f := one(t, a, RuleWillFail); f.Severity != changereview.SeverityCritical {
		t.Fatalf("dropping a constraint's index fails: CR-05 %s", f.Severity)
	}
}

func TestDropNeverScannedIndexIsInfoOnly(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE INDEX rv_orders_status_idx ON rv.rv_orders (status)")
	a := e.analyze("DROP INDEX CONCURRENTLY rv.rv_orders_status_idx")
	f := one(t, a, RuleIndexUsed)
	if f.Severity != changereview.SeverityInfo || indexOf(t, a, f).Scans != 0 {
		t.Fatalf("never-scanned index: CR-07 %+v, want info", f)
	}
	none(t, a, RuleWillFail)
	if verdict(a) != changereview.VerdictSafe {
		t.Fatalf("verdict %s, want safe: %s", verdict(a), describe(a))
	}
}

// CHECK-IMP-06: redundant and beneficial indexes.
func TestDuplicateIndexWarns(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("CREATE INDEX CONCURRENTLY rv_orders_created2_idx ON rv.rv_orders (created_at)")
	f := one(t, a, RuleIndexRedundant)
	if f.Severity != changereview.SeverityWarning ||
		!strings.Contains(f.Text, "rv_orders_created_idx") {
		t.Fatalf("CR-08 %+v, want a warning naming rv_orders_created_idx", f)
	}
}

func TestWiderIndexMakesTheExistingOneRedundant(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("CREATE INDEX CONCURRENTLY rv_orders_cs_idx ON rv.rv_orders " +
		"(created_at, status)")
	f := one(t, a, RuleIndexRedundant)
	if f.Severity != changereview.SeverityInfo ||
		!strings.Contains(f.Text, "rv_orders_created_idx") ||
		!strings.Contains(f.Text, "redundant") {
		t.Fatalf("CR-08 %+v, want info that rv_orders_created_idx becomes redundant", f)
	}
}

func TestHypoPGMeasuresTheIndexBenefit(t *testing.T) {
	e := newEnv(t)
	if n := e.f.Int64("SELECT count(*) FROM pg_extension WHERE extname = 'hypopg'"); n != 1 {
		t.Fatal("HypoPG is not installed in the test database (the CI images have it)")
	}
	for i := 1; i <= 20; i++ {
		e.f.Exec("SELECT id FROM rv.rv_orders WHERE customer_id = $1", i)
	}
	a := e.analyze("CREATE INDEX CONCURRENTLY rv_orders_cust_idx ON rv.rv_orders (customer_id)")
	f := one(t, a, RuleIndexBenefit)
	w := decode[whatIfData](t, citedOfKind(t, a, f, "whatif")[0])
	if w.Method != "hypopg" || !w.Available || w.ImprovementPct == nil ||
		*w.ImprovementPct <= 0 || w.Measured == 0 {
		t.Fatalf("CR-09 what-if %+v, want a HypoPG improvement > 0", w)
	}
	var predicted bool
	for _, p := range a.Expectations.Predictions {
		if p.Method == "hypopg" && p.ExpectedChangePct != nil && *p.ExpectedChangePct < 0 {
			predicted = true
		}
	}
	if !predicted {
		t.Fatalf("no HypoPG prediction among %+v", a.Expectations.Predictions)
	}
}

// unavailableWhatIf reports HypoPG missing; Validate must not be called.
type unavailableWhatIf struct{ t *testing.T }

func (u unavailableWhatIf) IsAvailable(context.Context) bool { return false }

func (u unavailableWhatIf) Validate(context.Context, optimizer.Recommendation,
	[]optimizer.QueryInfo) (optimizer.WhatIfResult, error) {
	u.t.Error("Validate called although HypoPG is unavailable")
	return optimizer.WhatIfResult{}, nil
}

func TestHypoPGUnavailableCarriesNoNumber(t *testing.T) {
	e := newEnv(t)
	eng := e.engine(e.f.Pool, func(d *Deps) { d.WhatIf = unavailableWhatIf{t} })
	a := e.analyzeWith(eng, input("CREATE INDEX CONCURRENTLY rv_orders_cust_idx ON "+
		"rv.rv_orders (customer_id)"))
	f := one(t, a, RuleIndexBenefit)
	w := decode[whatIfData](t, citedOfKind(t, a, f, "whatif")[0])
	if !strings.Contains(f.Text, "HypoPG unavailable") || w.ImprovementPct != nil ||
		w.Available || strings.Contains(string(citedOfKind(t, a, f, "whatif")[0].Data),
		"improvement_pct") {
		t.Fatalf("CR-09 %q / %+v, want 'HypoPG unavailable' and no number", f.Text, w)
	}
}

// CHECK-IMP-07: a new foreign key and its index and types.
func TestNewForeignKeyWithoutAnIndexWarns(t *testing.T) {
	e := newEnv(t)
	const fk = "ALTER TABLE rv.rv_items ADD CONSTRAINT rv_items_order_fk " +
		"FOREIGN KEY (order_id) REFERENCES rv.rv_orders (id)"
	a := e.analyze(fk)
	f := one(t, a, RuleFKUnindexed)
	k := decode[fkData](t, citedOfKind(t, a, f, "fk")[0])
	if f.Severity != changereview.SeverityWarning || k.RefRelation != "rv.rv_orders" ||
		!k.RateKnown || k.RefUpdPerS <= 0 || k.RefDelPerS != 0 {
		t.Fatalf("CR-10 %s %+v, want a warning with rv_orders' update and delete rate",
			f.Severity, k)
	}
	if !near(k.RefUpdPerS, float64(e.f.Q2.Calls)/7200, 0.02) {
		t.Fatalf("rv_orders update rate %v/s, want %v/s", k.RefUpdPerS,
			float64(e.f.Q2.Calls)/7200)
	}
	m := one(t, a, RuleFKTypeMismatch)
	if m.Severity != changereview.SeverityWarning || !strings.Contains(m.Text, "integer") ||
		!strings.Contains(m.Text, "bigint") {
		t.Fatalf("CR-11 %+v, want a warning naming integer and bigint", m)
	}
	e.f.Exec("CREATE INDEX rv_items_order_idx ON rv.rv_items (order_id)")
	b := e.analyze(fk)
	none(t, b, RuleFKUnindexed)
	if one(t, b, RuleFKTypeMismatch).Severity != changereview.SeverityWarning {
		t.Fatal("the type mismatch stays after the index exists")
	}
	c := e.analyze("ALTER TABLE rv.rv_items ADD COLUMN order_ref bigint " +
		"REFERENCES rv.rv_orders (id);\nCREATE INDEX CONCURRENTLY rv_items_ref_idx ON " +
		"rv.rv_items (order_ref)")
	none(t, c, RuleFKUnindexed, RuleFKTypeMismatch)
}
