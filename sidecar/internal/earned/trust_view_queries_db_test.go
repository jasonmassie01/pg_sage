package earned

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The Trust view is read by the Trust page and by every approval card, so
// it costs a constant number of set-based statements per database however
// many family x class pairs it shows or how many outcomes they have (never
// a statement per pair).

// trustViewMaxStatements bounds the statements of one annotated view.
const trustViewMaxStatements = 12

// tracedLedger is f's ledger read through a pool that records every
// statement it sends.
func tracedLedger(t *testing.T, f *fixture) (*Service, *Limiter, *testdb.QueryRecorder) {
	t.Helper()
	pc, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := &testdb.QueryRecorder{}
	pc.ConnConfig.Tracer = rec
	pc.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st, err := NewPostgresStore(pool, f.store.DeploymentID(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(st, f.svc.cfg)
	if err != nil {
		t.Fatal(err)
	}
	lim := svc.Limiter(Binding{Database: f.db,
		Budget: &fakeBudget{state: BudgetState{Configured: true}},
		HA:     &fakeHA{state: HAState{Role: RolePrimary}}, Concurrency: &fakeConcurrency{}})
	return svc, lim, rec
}

// annotatedView reads the view and its effective levels, returning the
// statements it took.
func annotatedView(t *testing.T, svc *Service, lim *Limiter,
	rec *testdb.QueryRecorder) (TrustView, int) {
	t.Helper()
	rec.Reset()
	v, err := svc.TrustView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lim.AnnotateTrust(context.Background(), &v)
	return v, len(rec.Matching())
}

// recordSpread records outcomes on many pairs of both kinds, a harmful
// one on the wraparound family included.
func recordSpread(t *testing.T, f *fixture) {
	t.Helper()
	at := f.clock.Now().Add(-time.Hour)
	outcomes := []Outcome{
		{Family: FamilyWraparound, Class: ClassFreeze, Result: ResultHarmful},
		{Family: FamilyWraparound, Class: ClassFreeze, Result: ResultVerifiedRecovery},
		{Family: FamilyLockBlocking, Class: ClassBackendCancel,
			Result: ResultVerifiedRecovery},
		{Family: FamilyTuning, Class: ClassIndexCreate, Result: ResultVerifiedRecovery,
			Verdict: "improved"},
		{Family: FamilyTuning, Class: ClassConfigGUC, Result: ResultHarmful,
			Verdict: "regressed"},
		{Family: FamilyHygiene, Class: ClassVacuum, Result: ResultVerifiedRecovery,
			Verdict: "neutral"},
		{Family: FamilyHygiene, Class: ClassAnalyze, Result: ResultRejected,
			Verdict: "rejected", Source: SourceOperator},
	}
	for i, o := range outcomes {
		o.Database, o.Level, o.Actor, o.At = f.db, L2, ActorPgSage, at
		o.ActionLogID = int64(9_000_000 + i)
		ts := at
		o.ObservedAt = &ts
		if o.Source == "" {
			o.Source = SourceExecutor
		}
		if _, err := f.store.insertOutcome(f.ctx, o); err != nil {
			t.Fatalf("outcome %d: %v", i, err)
		}
	}
}

func TestTrustViewReadsAConstantNumberOfStatements(t *testing.T) {
	f := newReconFixture(t)
	svc, lim, rec := tracedLedger(t, f)
	empty, before := annotatedView(t, svc, lim, rec)
	if len(empty.Rows) < 30 {
		t.Fatalf("view has %d rows; the test needs the full grid", len(empty.Rows))
	}
	if before > trustViewMaxStatements {
		t.Fatalf("an empty view took %d statements, want <= %d:\n%s", before,
			trustViewMaxStatements, statementList(rec))
	}
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db,
		rampBound(f.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recordSpread(t, f)
	v, after := annotatedView(t, svc, lim, rec)
	if after != before {
		t.Fatalf("statements grew with the data: %d before, %d after:\n%s", before, after,
			statementList(rec))
	}
	guc, _ := trustRow(v, FamilyTuning, ClassConfigGUC)
	if guc.Evidence.Regressed != 1 {
		t.Fatalf("tuning/config_guc = %+v", guc)
	}
	freeze, _ := trustRow(v, FamilyWraparound, ClassFreeze)
	if !hasDowngrade(freeze.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("wraparound row lacks the family's safety signal: %+v", freeze.Downgrades)
	}
	other, _ := trustRow(v, FamilyLockBlocking, ClassBackendCancel)
	if hasDowngrade(other.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("another family took wraparound's safety signal: %+v", other.Downgrades)
	}
}

// The set-based view must say exactly what the per-pair reads say: the
// stored level, the counts and the assessment of the next level.
func TestTrustViewAgreesWithPerPairReads(t *testing.T) {
	f := newReconFixture(t)
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db,
		rampBound(f.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recordSpread(t, f)
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range v.Rows {
		st, err := f.svc.Granted(f.ctx, row.Family, row.Class)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := f.store.ClassRecord(f.ctx, row.Family, row.Class)
		if err != nil {
			t.Fatal(err)
		}
		if st.Level != row.Level || st.Provenance != row.Provenance ||
			countsOf(rec) != row.Evidence {
			t.Fatalf("%s/%s: view %+v, per pair level %v %s counts %+v", row.Family,
				row.Class, row, st.Level, st.Provenance, countsOf(rec))
		}
		if row.Next == nil {
			continue
		}
		ev, err := f.svc.Evidence(f.ctx, row.Family, row.Class)
		if err != nil {
			t.Fatal(err)
		}
		checkPairReads(t, f, row, ev)
		want := Assess(f.svc.cfg.Thresholds, row.Next.Target, ev)
		if !reflect.DeepEqual(want, *row.Next) {
			t.Fatalf("%s/%s next: view %+v, per pair %+v", row.Family, row.Class,
				*row.Next, want)
		}
	}
}

func hasDowngrade(ds []Downgrade, signal string) bool {
	for _, d := range ds {
		if d.Reason == signal {
			return true
		}
	}
	return false
}

func statementList(rec *testdb.QueryRecorder) string {
	var b strings.Builder
	for _, q := range rec.Matching() {
		line := strings.Join(strings.Fields(q.SQL), " ")
		if len(line) > 140 {
			line = line[:140]
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// checkPairReads holds a pair's evidence against the store's own reads of
// that pair, so a set row is never attributed to another pair.
func checkPairReads(t *testing.T, f *fixture, row TrustRow, ev Evidence) {
	t.Helper()
	if IsSelfInitiated(row.Family) {
		return
	}
	live, err := f.store.LiveStats(f.ctx, row.Family, row.Class)
	if err != nil {
		t.Fatal(err)
	}
	since := f.clock.Now().Add(-f.svc.cfg.SafetyWindow)
	n, err := f.store.FamilyViolations(f.ctx, row.Family, since)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Live != live || ev.FamilyViolations != n {
		t.Fatalf("%s/%s: evidence live %+v violations %d, store %+v %d", row.Family,
			row.Class, ev.Live, ev.FamilyViolations, live, n)
	}
}
