package shadow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Integration (real Postgres), security-01c-n: a decision's SQL names its
// table as the application writes it ("orders"), and pg_sage's session is
// pinned to pg_catalog, pg_temp. The detectors resolve the name on the
// application's path (app, public), never on the pin (nothing) and never
// on the decoy sage.orders. No concurrent-access test: each read has its
// own transaction and the path is transaction-local.

func appPathScorer(t *testing.T, label string) (*Scorer, func(string)) {
	t.Helper()
	admin, pinned := testdb.AppPathDatabase(t, label)
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return NewScorer(pinned, fastOptions(), nil), exec
}

func pendingOn(class, sql string) pendingDecision {
	return pendingDecision{Decision: Decision{ID: 1, Class: class, SQL: sql,
		RecordedAt: time.Now(), Prediction: verify.Prediction{
			Metric: verify.MetricDeadTuples}}}
}

func assertNotGone(t *testing.T, what string, f *verifiedFact, own bool, err error) {
	t.Helper()
	if err != nil || own || f != nil {
		t.Fatalf("%s = %+v, own=%v, err=%v; want nothing applied (the table resolves)",
			what, f, own, err)
	}
}

// Happy path and boundary: an unqualified reloption decision reads
// app.orders, and the same option on the decoy sage.orders is no match.
func TestReloptionExternal_ResolvesTheTableOnTheApplicationPath(t *testing.T) {
	sc, exec := appPathScorer(t, "shadow_apppath_rel")
	exec(`ALTER TABLE sage.orders SET (autovacuum_vacuum_scale_factor = 0.05)`)
	p := pendingOn("autovacuum_tuning",
		`ALTER TABLE orders SET (autovacuum_vacuum_scale_factor = 0.05)`)
	f, own, err := sc.reloptionExternal(context.Background(), p)
	assertNotGone(t, "reloptionExternal(orders)", f, own, err)
}

// Invalid input: a table that is not on the application's path is gone,
// named as the decision wrote it.
func TestReloptionExternal_MissingTableIsGone(t *testing.T) {
	sc, _ := appPathScorer(t, "shadow_apppath_gone")
	p := pendingOn("autovacuum_tuning",
		`ALTER TABLE missing SET (autovacuum_vacuum_scale_factor = 0.05)`)
	f, _, err := sc.reloptionExternal(context.Background(), p)
	if err != nil || f == nil || !strings.Contains(f.Unscorable, "relation gone: missing") {
		t.Fatalf("reloptionExternal(missing) = %+v, %v; want relation gone", f, err)
	}
}

// serverNow is the server's now(), the clock that dates a recorded decision
// (sage.shadow_decision.recorded_at DEFAULT now()). The maintenance read
// compares the decision's time with server timestamps (stats_reset,
// last_vacuum), so a decision dated by the test host's clock is wrong
// whenever the two clocks differ.
func serverNow(t *testing.T, sc *Scorer) time.Time {
	t.Helper()
	var now time.Time
	if err := sc.catalog.QueryRow(context.Background(), "SELECT pg_catalog.now()").
		Scan(&now); err != nil {
		t.Fatalf("read the server clock: %v", err)
	}
	return now
}

// Boundary: a quoted mixed-case name resolves exactly.
func TestMaintenanceExternal_ResolvesAQuotedTable(t *testing.T) {
	sc, _ := appPathScorer(t, "shadow_apppath_mnt")
	for _, sql := range []string{`VACUUM orders`, `VACUUM "Order Items"`} {
		p := pendingOn("vacuum", sql)
		p.RecordedAt = serverNow(t, sc)
		f, own, err := sc.maintenanceExternal(context.Background(), p)
		assertNotGone(t, "maintenanceExternal("+sql+")", f, own, err)
	}
}

// Error propagation: a failed resolution is an error that names the
// relation, not a verdict. (The index detectors need no resolution: Shape
// names an unqualified index or table with public.)
func TestReloptionExternal_ResolveFailureNamesTheTable(t *testing.T) {
	sc, _ := appPathScorer(t, "shadow_apppath_err")
	sc.pool.Close()
	p := pendingOn("autovacuum_tuning",
		`ALTER TABLE orders SET (autovacuum_vacuum_scale_factor = 0.05)`)
	f, _, err := sc.reloptionExternal(context.Background(), p)
	if err == nil || f != nil || !strings.Contains(err.Error(), "resolve orders") {
		t.Fatalf("reloptionExternal on a closed pool = %+v, %v; want an error naming orders",
			f, err)
	}
}
