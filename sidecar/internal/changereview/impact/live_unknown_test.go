//go:build cgo

package impact

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

func flushStats(e *env) error { return testdb.FlushIdleSessions(e.ctx, e.f.Pool) }

// rvReaderWithout is pg_sage's monitoring role with every rv table but
// the given ones readable.
func rvReaderWithout(tables ...string) rvfixture.Reader {
	r := rvfixture.Reader{ReadAllStats: true}
	for _, t := range []string{"rv_orders", "rv_items"} {
		if !contains(tables, t) {
			r.SelectTables = append(r.SelectTables, t)
		}
	}
	return r
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// unknownWith is the CR-17 warnings whose title or text mention needle.
func unknownWith(a changereview.Analysis, needle string) []changereview.Finding {
	var out []changereview.Finding
	for _, f := range byRule(a, RuleImpactUnknown) {
		if strings.Contains(f.Title+" "+f.Text, needle) &&
			f.Severity == changereview.SeverityWarning {
			out = append(out, f)
		}
	}
	return out
}

// CHECK-IMP-13: unknown is never none.
func TestWithoutPgStatStatementsReferencesAreUnknown(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("DROP EXTENSION pg_stat_statements")
	a := e.analyze("ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address;\n" +
		"DROP TABLE rv.rv_items")
	none(t, a, RuleColumnReferenced, RuleRelationReferenced)
	if len(unknownWith(a, "pg_stat_statements")) < 2 {
		t.Fatalf("want CR-17 warnings for the column and the table: %s", describe(a))
	}
	if v := verdict(a); v == changereview.VerdictSafe {
		t.Fatalf("verdict %s, want at least review", v)
	}
}

func TestHiddenQueryTextIsUnknown(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.f.ReaderPool(rvfixture.Reader{SelectTables: []string{"rv_orders",
		"rv_items"}})
	a := e.analyzeWith(e.engine(pool, nil),
		input("ALTER TABLE rv.rv_orders DROP COLUMN status"))
	none(t, a, RuleColumnReferenced)
	fs := unknownWith(a, "hidden")
	if len(fs) == 0 {
		t.Fatalf("want CR-17 'query text hidden': %s", describe(a))
	}
	if !strings.Contains(fs[0].Text, "pg_read_all_stats") {
		t.Fatalf("CR-17 %q must name the grant that fixes it", fs[0].Text)
	}
	if verdict(a) == changereview.VerdictSafe {
		t.Fatal("hidden statement text must never be safe")
	}
}

func TestSelectStarReadsEveryColumn(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("SELECT * FROM rv.rv_orders WHERE id = $1", int64(7))
	a := e.analyze("ALTER TABLE rv.rv_orders DROP COLUMN code")
	if len(unknownWith(a, "reads every column")) != 1 {
		t.Fatalf("want one CR-17 'reads every column': %s", describe(a))
	}
	b := e.analyze("CREATE INDEX CONCURRENTLY rv_orders_code_idx ON rv.rv_orders (code)")
	if len(unknownWith(b, "reads every column")) != 0 {
		t.Fatalf("an index build does not change the columns a SELECT * reads")
	}
}

func TestBlockedCatalogReadIsTimedOutEvidence(t *testing.T) {
	e := newEnv(t)
	locker, err := pgx.Connect(e.ctx, e.f.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Close(context.Background()) }()
	tx, err := locker.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(e.ctx, "LOCK TABLE rv.rv_orders IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	eng := e.engine(e.f.Pool, func(d *Deps) {
		d.Timeouts = catalogread.Timeouts{Statement: time.Second, Lock: 200 * time.Millisecond}
		d.ExplainTimeout = time.Second
	})
	start := time.Now()
	a, err := eng.Analyze(e.ctx, input("ALTER TABLE rv.rv_orders ADD COLUMN x int"))
	if err != nil {
		t.Fatalf("a blocked catalog read must not fail the review: %v", err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("Analyze took %v behind the lock", took)
	}
	fs := unknownWith(a, "timed out")
	if len(fs) == 0 {
		t.Fatalf("want CR-17 'timed out': %s", describe(a))
	}
	var timedOut bool
	for _, ev := range citedOfKind(t, a, fs[0], "unknown") {
		timedOut = timedOut || decode[unknownData](t, ev).Reason == "timed out"
	}
	if !timedOut {
		t.Fatalf("CR-17 cites no 'timed out' evidence: %v", fs[0].EvidenceIDs)
	}
	if verdict(a) == changereview.VerdictSafe {
		t.Fatal("a timed-out read must never be safe")
	}
}

func TestCancelledContextIsAnError(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	_, err := e.eng.Analyze(ctx, input("ALTER TABLE rv.rv_orders ADD COLUMN note text"))
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Analyze on a cancelled context: %v, want a context error", err)
	}
}

func TestUnknownRelationIsUnknownNotNone(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.missing_t DROP COLUMN a")
	if len(unknownWith(a, "rv.missing_t")) == 0 {
		t.Fatalf("a relation the catalog does not hold must be CR-17: %s", describe(a))
	}
	if verdict(a) == changereview.VerdictSafe {
		t.Fatal("DDL on an unknown relation must never be safe")
	}
}
