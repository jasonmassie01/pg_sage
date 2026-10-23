//go:build cgo

package impact

import (
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/migration"
)

// lockQueueOf is the single CR-02 finding and its lock-queue evidence.
func lockQueueOf(t *testing.T, a changereview.Analysis) (changereview.Finding,
	lockQueueData) {
	t.Helper()
	f := one(t, a, RuleLockQueue)
	ev := citedOfKind(t, a, f, "lockq")
	if len(ev) != 1 {
		t.Fatalf("CR-02 cites %d lock-queue evidence, want 1", len(ev))
	}
	return f, decode[lockQueueData](t, ev[0])
}

// CHECK-IMP-04: the lock and the queue behind it.
func TestRewriteTypeChangeLockQueue(t *testing.T) {
	e := newEnv(t)
	const sql = "ALTER TABLE rv.rv_orders ALTER COLUMN status TYPE varchar(20)"
	a := e.analyze(sql)
	cr01 := byRule(a, RuleLockAndRewrite)
	if len(cr01) == 0 {
		t.Fatalf("no CR-01: %s", describe(a))
	}
	risk := decode[riskData](t, citedOfKind(t, a, cr01[0], "risk")[0])
	if risk.LockLevel != lockAccessExclusive || !risk.RequiresRewrite {
		t.Fatalf("CR-01 %+v, want ACCESS EXCLUSIVE with a rewrite", risk)
	}
	f, q := lockQueueOf(t, a)
	gap := e.f.SampledTo.Sub(e.f.SampledFrom).Seconds()
	want := float64(e.f.Q1.Calls+e.f.Q2.Calls+e.f.Q3.Calls) / gap
	if !near(q.PeakConflictingPerS, want, 0.01) {
		t.Fatalf("peak conflicting rate %v/s, want %v/s within 1%%", q.PeakConflictingPerS,
			want)
	}
	ids := append([]int64(nil), q.ConflictingQueryIDs...)
	slices.Sort(ids)
	wantIDs := []int64{e.f.Q1.ID, e.f.Q2.ID, e.f.Q3.ID}
	slices.Sort(wantIDs)
	if !slices.Equal(ids, wantIDs) {
		t.Fatalf("conflicting queryids %v, want Q1..Q3 %v", ids, wantIDs)
	}
	if hold := assessedLockMs(t, e, sql); q.HoldMs != hold || q.HoldSource != holdRiskAssessor {
		t.Fatalf("hold %d ms from %s, want RiskAssessor's %d ms", q.HoldMs, q.HoldSource, hold)
	}
	if !near(q.Queued, want*float64(q.HoldMs)/1000, 0.0001) || q.Method != "rule" {
		t.Fatalf("queued %v (%s), want rate x hold", q.Queued, q.Method)
	}
	if !q.NoLockTimeout || q.WaitingExposure != nil || !strings.Contains(f.Text,
		"no lock_timeout") {
		t.Fatalf("missing lock_timeout must be flagged and unbounded: %+v %q", q, f.Text)
	}
}

// assessedLockMs is what migration.RiskAssessor estimates for sql.
func assessedLockMs(t *testing.T, e *env, sql string) int64 {
	t.Helper()
	ra := migration.NewRiskAssessor(e.f.Pool, func(string, string, ...any) {})
	ra.SetCatalogReadTimeouts(catalogread.Default())
	var hold int64
	for _, c := range migration.NewRegexClassifier().Classify(sql, 170000) {
		if c.RuleID == "ddl_missing_lock_timeout" {
			continue
		}
		r, err := ra.Assess(e.ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		hold = max(hold, r.EstimatedLockMs)
	}
	if hold == 0 {
		t.Fatalf("RiskAssessor scored nothing for %q", sql)
	}
	return hold
}

func TestLockTimeoutBoundsTheWaitingExposure(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("SET lock_timeout = '2s';\n" +
		"ALTER TABLE rv.rv_orders ALTER COLUMN status TYPE varchar(20)")
	f, q := lockQueueOf(t, a)
	if q.LockTimeoutMs == nil || *q.LockTimeoutMs != 2000 || q.NoLockTimeout {
		t.Fatalf("lock_timeout %v (no_lock_timeout %v), want 2000 ms", q.LockTimeoutMs,
			q.NoLockTimeout)
	}
	if q.WaitingExposure == nil || !near(*q.WaitingExposure, q.PeakConflictingPerS*2, 1e-9) {
		t.Fatalf("waiting exposure %v, want rate x 2 s = %v", q.WaitingExposure,
			q.PeakConflictingPerS*2)
	}
	if strings.Contains(f.Text, "no lock_timeout") {
		t.Fatalf("CR-02 still flags a missing lock_timeout: %q", f.Text)
	}
}

func TestPlainCreateIndexConflictsOnlyWithWrites(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("CREATE INDEX rv_orders_status_idx ON rv.rv_orders (status)")
	_, q := lockQueueOf(t, a)
	if !slices.Equal(q.ConflictingQueryIDs, []int64{e.f.Q2.ID}) {
		t.Fatalf("conflicting %v, want only the write Q2 %d", q.ConflictingQueryIDs,
			e.f.Q2.ID)
	}
	if q.LockMode != lockShare || q.HoldSource != holdIndexBuild || q.Method != "rule" {
		t.Fatalf("CREATE INDEX lock %s hold from %s (%s), want SHARE over the heap", q.LockMode,
			q.HoldSource, q.Method)
	}
	heap := e.f.Int64("SELECT pg_relation_size('rv.rv_orders')")
	if q.HoldMs != indexBuildHoldMs(heap) || q.HoldMs <= 0 {
		t.Fatalf("hold %d ms, want heap %d bytes / 50 MiB/s = %d", q.HoldMs, heap,
			indexBuildHoldMs(heap))
	}
	if !near(q.PeakConflictingPerS, e.f.Q2.RatePerSecond(), 0.01) {
		t.Fatalf("rate %v, want Q2's %v", q.PeakConflictingPerS, e.f.Q2.RatePerSecond())
	}
}

func TestBinaryCoercibleTypeChangeDoesNotRewrite(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN code TYPE varchar(40)")
	f := one(t, a, RuleTypeChange)
	if !strings.Contains(f.Title+f.Text, "binary-coercible: no rewrite") ||
		f.Severity != changereview.SeverityInfo {
		t.Fatalf("CR-06 %+v, want info 'binary-coercible: no rewrite'", f)
	}
	for _, cr01 := range byRule(a, RuleLockAndRewrite) {
		for _, ev := range citedOfKind(t, a, cr01, "risk") {
			if decode[riskData](t, ev).RequiresRewrite {
				t.Fatalf("CR-01 claims a rewrite for a binary-coercible change")
			}
		}
	}
	b := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN code TYPE bigint USING 0")
	if f := one(t, b, RuleTypeChange); f.Severity != changereview.SeverityWarning ||
		!strings.Contains(f.Text, "rewrite") {
		t.Fatalf("a rewriting type change: CR-06 %+v, want a warning about the rewrite", f)
	}
}

// CHECK-IMP-09: CONCURRENTLY cannot run inside a transaction block.
func TestConcurrentIndexInATransactionBlockWillFail(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("BEGIN;\nCREATE INDEX CONCURRENTLY rv_orders_cust_idx ON " +
		"rv.rv_orders (customer_id);\nCOMMIT;")
	f := one(t, a, RuleWillFail)
	if f.Severity != changereview.SeverityCritical ||
		!strings.Contains(f.Text, "transaction block") {
		t.Fatalf("CR-05 %+v, want critical about the transaction block", f)
	}
	if !cites(f, "stmt:2") {
		t.Fatalf("CR-05 must cite the CREATE INDEX (stmt:2): %v", f.EvidenceIDs)
	}
}

func TestConcurrentIndexInATransactionalFile(t *testing.T) {
	e := newEnv(t)
	const sql = "CREATE INDEX CONCURRENTLY rv_orders_cust_idx ON rv.rv_orders (customer_id)"
	for _, tool := range []string{"rails", "django", "alembic"} {
		mt, ok := changereview.LookupMigrationTool(tool)
		if !ok || !mt.Transactional {
			t.Fatalf("%s must be transactional by default", tool)
		}
	}
	if mt, _ := changereview.LookupMigrationTool("flyway"); mt.Transactional {
		t.Fatal("flyway is not transactional by default")
	}
	in := input(sql)
	in.MigrationTool, in.Transactional = "rails", true
	a := e.analyzeWith(e.eng, in)
	if f := one(t, a, RuleWillFail); f.Severity != changereview.SeverityCritical ||
		!strings.Contains(f.Text, "rails") {
		t.Fatalf("transactional rails file: CR-05 %+v, want critical naming rails", f)
	}
	in.MigrationTool, in.Transactional = "flyway", false
	none(t, e.analyzeWith(e.eng, in), RuleWillFail)
	in.MigrationTool, in.Transactional = "rails", false
	in.TransactionalFrom = changereview.TransactionalFromRequest
	none(t, e.analyzeWith(e.eng, in), RuleWillFail)
}
