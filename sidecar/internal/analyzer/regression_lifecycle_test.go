package analyzer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Regression tests for the finding lifecycle bugs recorded in
// reviews/2026-09-26 (G2-B01/C02, G2-B02/C03, C04, G2-B06/G7-B07).

func lifecycleFinding(cat, ident, sev string) Finding {
	return Finding{
		Category:         cat,
		Severity:         sev,
		ObjectType:       "table",
		ObjectIdentifier: ident,
		Title:            "lifecycle " + ident,
		Detail:           map[string]any{"k": "v"},
		Recommendation:   "fix it",
		RecommendedSQL:   "VACUUM public.t;",
	}
}

func countFindingRows(
	t *testing.T, pool *pgxpool.Pool, cat, ident, status string,
) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2
		    AND status = $3`, cat, ident, status).Scan(&n)
	if err != nil {
		t.Fatalf("count findings: %v", err)
	}
	return n
}

func suppressFinding(t *testing.T, pool *pgxpool.Pool, cat, ident string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE sage.findings SET status = 'suppressed'
		  WHERE category = $1 AND object_identifier = $2
		    AND status = 'open'`, cat, ident)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("suppress: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

func lifecycleAnalyzer(t *testing.T) (*Analyzer, *pgxpool.Pool, *mockDispatcher) {
	t.Helper()
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	d := &mockDispatcher{}
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	return a, pool, d
}

// G2-B01/C02: a suppressed identity must not be re-opened by the next
// observation, and must not be handed to the executor (in-memory list).
func TestRegression_SuppressionIsDurable(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_suppress", "public.orders", "warning")
	eval := map[string]bool{f.Category: true}

	a.finalizeCycle(ctx, []Finding{f}, eval)
	suppressFinding(t, pool, f.Category, f.ObjectIdentifier)

	for i := 0; i < 3; i++ {
		a.finalizeCycle(ctx, []Finding{f}, eval)
	}
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 0 {
		t.Fatalf("open rows after suppression = %d, want 0", got)
	}
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "suppressed"); got != 1 {
		t.Fatalf("suppressed rows = %d, want 1", got)
	}
	for _, lf := range a.LatestFindings() {
		if lf.Category == f.Category && lf.ObjectIdentifier == f.ObjectIdentifier {
			t.Fatalf("suppressed finding exposed to executor: %+v", lf)
		}
	}
}

// G2-B01/C02: an expired suppression re-opens exactly one identity.
func TestRegression_ExpiredSuppressionReopensOnce(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_suppress_exp", "public.items", "warning")
	eval := map[string]bool{f.Category: true}

	a.finalizeCycle(ctx, []Finding{f}, eval)
	_, err := pool.Exec(ctx,
		`UPDATE sage.findings
		    SET status = 'suppressed',
		        suppressed_until = now() - interval '1 minute'
		  WHERE category = $1 AND status = 'open'`, f.Category)
	if err != nil {
		t.Fatalf("expire suppression: %v", err)
	}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	a.finalizeCycle(ctx, []Finding{f}, eval)
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 1 {
		t.Fatalf("open rows after expiry = %d, want 1", got)
	}
}

// C04: refreshing an open finding must refresh rollback_sql together with
// recommended_sql so the pair never mixes proposals.
func TestRegression_UpsertRefreshesRollbackSQL(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ctx := context.Background()

	f := lifecycleFinding("test_phase2_c04", "public.orders", "info")
	f.RecommendedSQL = "CREATE INDEX CONCURRENTLY index_a ON public.orders (a);"
	f.RollbackSQL = "DROP INDEX CONCURRENTLY public.index_a;"
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	f.RecommendedSQL = "CREATE INDEX CONCURRENTLY index_b ON public.orders (b);"
	f.RollbackSQL = "DROP INDEX CONCURRENTLY public.index_b;"
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	var fwd, inv string
	err := pool.QueryRow(ctx,
		`SELECT recommended_sql, rollback_sql FROM sage.findings
		  WHERE category = $1 AND status = 'open'`, f.Category,
	).Scan(&fwd, &inv)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if fwd != f.RecommendedSQL || inv != f.RollbackSQL {
		t.Fatalf("persisted pair = (%q, %q), want (%q, %q)",
			fwd, inv, f.RecommendedSQL, f.RollbackSQL)
	}
}

// G2-B02/C03: the last finding of a successfully evaluated category must
// resolve when the category produces nothing; a failed evaluator must not
// resolve anything.
func TestRegression_LastFindingInCategoryResolves(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_b02", "public.orders", "warning")

	a.finalizeCycle(ctx, []Finding{f}, map[string]bool{f.Category: true})
	// Evaluator failed this cycle: category not evaluated → untouched.
	a.finalizeCycle(ctx, nil, map[string]bool{})
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 1 {
		t.Fatalf("open rows after failed evaluation = %d, want 1", got)
	}
	// Evaluated successfully with an empty result → resolved.
	a.finalizeCycle(ctx, nil, map[string]bool{f.Category: true})
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 0 {
		t.Fatalf("open rows after empty evaluation = %d, want 0", got)
	}
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved"); got != 1 {
		t.Fatalf("resolved rows = %d, want 1", got)
	}
}

// G2-B06/G7-B07: a persistent critical finding is notified once, not
// every cycle; a severity escalation notifies again.
func TestRegression_CriticalNotifiedOnlyWhenNew(t *testing.T) {
	a, _, d := lifecycleAnalyzer(t)
	ctx := context.Background()
	crit := lifecycleFinding("test_phase2_b06", "public.orders", "critical")
	eval := map[string]bool{crit.Category: true}

	for i := 0; i < 3; i++ {
		a.finalizeCycle(ctx, []Finding{crit}, eval)
	}
	if got := d.count(); got != 1 {
		t.Fatalf("dispatches after 3 identical cycles = %d, want 1", got)
	}

	warn := lifecycleFinding("test_phase2_b06_esc", "public.items", "warning")
	eval[warn.Category] = true
	a.finalizeCycle(ctx, []Finding{crit, warn}, eval)
	warn.Severity = "critical"
	a.finalizeCycle(ctx, []Finding{crit, warn}, eval)
	if got := d.count(); got != 2 {
		t.Fatalf("dispatches after escalation = %d, want 2", got)
	}
}

// G2-B01: suppressed critical findings are never paged.
func TestRegression_SuppressedCriticalNotNotified(t *testing.T) {
	a, pool, d := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_b01_notify", "public.orders", "warning")
	eval := map[string]bool{f.Category: true}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	suppressFinding(t, pool, f.Category, f.ObjectIdentifier)
	f.Severity = "critical"
	a.finalizeCycle(ctx, []Finding{f}, eval)
	if got := d.count(); got != 0 {
		t.Fatalf("dispatches for suppressed finding = %d, want 0", got)
	}
}

// G2-B03: the XID wraparound finding must not be classified as pg_sage
// self-monitoring (which silently drops it from persistence).
func TestRegression_XIDWraparoundFindingPersists(t *testing.T) {
	cfg := phase2Config()
	ff := ruleXIDWraparound(cfg.Analyzer.XIDWraparoundCritical+1, cfg)
	if len(ff) != 1 {
		t.Fatalf("findings = %d, want 1", len(ff))
	}
	if isSelfMonitoringFinding(ff[0]) {
		t.Fatalf("xid_wraparound classified as self-monitoring: sql=%q",
			ff[0].RecommendedSQL)
	}
	if ff[0].Severity != "critical" {
		t.Fatalf("severity = %q, want critical", ff[0].Severity)
	}
}

// G2-B03 (integration): the finding lands in sage.findings.
func TestRegression_XIDWraparoundFindingUpserted(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()
	cfg := phase2Config()
	ff := ruleXIDWraparound(cfg.Analyzer.XIDWraparoundCritical+1, cfg)
	ff[0].Category = "test_phase2_xid"
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	if err := UpsertFindings(ctx, pool, ff); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := countFindingRows(t, pool, "test_phase2_xid", "current_database", "open"); got != 1 {
		t.Fatalf("persisted xid findings = %d, want 1", got)
	}
}
