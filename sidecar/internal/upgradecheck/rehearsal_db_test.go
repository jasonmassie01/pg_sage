package upgradecheck

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-02b: an upgrade rehearsal clones the database (receipt purpose
// rehearsal), captures the top statements' plans and blocks per call,
// upgrades the clone, captures again as cut over and after ANALYZE, and
// destroys the clone.

func seedTarget(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the upgraded copy: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, s := range seedSQL {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func serverMajor(t *testing.T, dsn string) int {
	t.Helper()
	return int(queryInt(t, dsn, "SELECT current_setting('server_version_num')::int") / 10000)
}

func queryInt(t *testing.T, dsn, sql string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var v int64
	if err := conn.QueryRow(ctx, sql).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func stateNames(r RehearsalReport) []string {
	out := make([]string, len(r.Steps))
	for i, s := range r.Steps {
		out[i] = s.State
	}
	return out
}

func findQuery(qs []RehearsedQuery, id int64) (RehearsedQuery, bool) {
	for _, q := range qs {
		if q.QueryID == id {
			return q, true
		}
	}
	return RehearsedQuery{}, false
}

func runFixture(t *testing.T, f rehearsalFixture, up UpgradeRehearser) (RehearsalReport,
	error) {
	t.Helper()
	p := f.receipting(t, f.control.ctx, 0)
	return Rehearse(f.control.ctx, f.options(t, p, up))
}

// CHECK-36: the skewed statement's plan changes as cut over (no
// statistics) and recovers after ANALYZE, with blocks per call deltas.
func TestRehearsalDB_FixtureListsTheChangedPlanWithBlocksDelta(t *testing.T) {
	f := newRehearsalFixture(t)
	rep, err := runFixture(t, f, &fixtureRehearser{dsn: f.targetDSN})
	if err != nil {
		t.Fatalf("Rehearse: %v (report %+v)", err, rep)
	}
	q, ok := findQuery(rep.Changed, 1)
	if !ok {
		t.Fatalf("skewed statement not listed as changed: changed %+v, not compared %+v",
			rep.Changed, rep.NotCompared)
	}
	if !q.ChangedAsCutOver || q.ChangedAfterAnalyze || !q.RecoveredAfterAnalyze() {
		t.Fatalf("skewed statement = %+v, want changed as cut over, recovered after ANALYZE",
			q)
	}
	if q.BlocksDeltaAsCutOver == nil || *q.BlocksDeltaAsCutOver <= 0 {
		t.Fatalf("blocks delta as cut over = %v, want a rise (bitmap scan of the whole table)",
			q.BlocksDeltaAsCutOver)
	}
	if q.BlocksDeltaAfterAnalyze == nil {
		t.Fatalf("blocks delta after ANALYZE missing: %+v", q)
	}
	if q.Before.PlanHash == "" || q.Before.PlanHash == q.AsCutOver.PlanHash ||
		q.Before.PlanHash != q.AfterAnalyze.PlanHash {
		t.Fatalf("plan hashes before %q, cut over %q, after %q", q.Before.PlanHash,
			q.AsCutOver.PlanHash, q.AfterAnalyze.PlanHash)
	}
	if _, listed := findQuery(rep.Changed, 2); listed || rep.Unchanged < 1 {
		t.Fatalf("stable statement listed as changed (unchanged %d): %+v", rep.Unchanged,
			rep.Changed)
	}
	if rep.PlansChanged != len(rep.Changed) || rep.RecoveredAfterAnalyze < 1 {
		t.Fatalf("counts changed %d (list %d), recovered %d", rep.PlansChanged,
			len(rep.Changed), rep.RecoveredAfterAnalyze)
	}
	assertFixtureSteps(t, f, rep)
}

func assertFixtureSteps(t *testing.T, f rehearsalFixture, rep RehearsalReport) {
	t.Helper()
	want := []string{RehearsalRequested, RehearsalCloned, RehearsalUpgraded,
		RehearsalAnalyzed, RehearsalDestroyed}
	if got := stateNames(rep); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if rep.State != RehearsalDestroyed || rep.Error != "" || rep.CleanupError != "" {
		t.Fatalf("final state %q error %q cleanup %q", rep.State, rep.Error, rep.CleanupError)
	}
	if rep.UpgradeAt.IsZero() || rep.AnalyzeAt.IsZero() || rep.UpgradeSeconds < 0 ||
		rep.AnalyzeSeconds <= 0 || rep.ReceiptID <= 0 || rep.CloneID == "" {
		t.Fatalf("timings/receipt: %+v", rep)
	}
	if got := f.inner.destroyedIDs(); !slices.Equal(got, []string{rep.CloneID}) {
		t.Fatalf("destroyed %v, want the clone %s once", got, rep.CloneID)
	}
	if rep.SourceMajor != f.sourceMajor || rep.TargetMajor != f.target {
		t.Fatalf("majors %d->%d, want %d->%d", rep.SourceMajor, rep.TargetMajor,
			f.sourceMajor, f.target)
	}
}

// Boundary: statements not provably read-only are listed as skipped and
// never executed on the clone; a parameterized one is not comparable
// below PG16 on either side.
func TestRehearsalDB_UnprovenStatementsAreSkippedNeverExecuted(t *testing.T) {
	f := newRehearsalFixture(t)
	rep, err := runFixture(t, f, &fixtureRehearser{dsn: f.targetDSN})
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	for _, id := range []int64{3, 4, 6} {
		q, ok := findQuery(rep.NotCompared, id)
		if !ok || q.Status != QuerySkipped || q.Reason == "" {
			t.Fatalf("statement %d = %+v (listed %v), want skipped with a reason", id, q, ok)
		}
		if q.Before != nil || q.AsCutOver != nil || q.AfterAnalyze != nil {
			t.Fatalf("skipped statement %d has captures: %+v", id, q)
		}
	}
	for _, dsn := range []string{f.sourceDSN, f.targetDSN} {
		if n := queryInt(t, dsn, "SELECT count(*) FROM public.calls"); n != 0 {
			t.Fatalf("public.calls has %d rows on %s: a skipped statement ran", n, dsn)
		}
	}
	q, ok := findQuery(append(rep.NotCompared, rep.Changed...), 5)
	switch {
	case f.sourceMajor < 16 && (!ok || q.Status != QueryNotComparable ||
		!strings.Contains(q.Reason, "16")):
		t.Fatalf("parameterized statement on PG%d = %+v, want not comparable (PG16)",
			f.sourceMajor, q)
	case f.sourceMajor >= 16 && ok && q.Status != QueryCompared:
		t.Fatalf("parameterized statement on PG%d = %+v, want compared by generic plan",
			f.sourceMajor, q)
	}
}

// Nil/empty: no top statements is "nothing to compare", and the upgrade
// and ANALYZE are still timed.
func TestRehearsalDB_NoTopStatementsStillTimesTheUpgrade(t *testing.T) {
	f := newRehearsalFixture(t)
	opts := f.options(t, f.receipting(t, f.control.ctx, 0),
		&fixtureRehearser{dsn: f.targetDSN})
	opts.Top = func(context.Context, int) ([]TopQuery, error) { return nil, nil }
	rep, err := Rehearse(f.control.ctx, opts)
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if rep.Note != NothingToCompare || len(rep.Changed) != 0 || len(rep.NotCompared) != 0 {
		t.Fatalf("report = %+v, want %q and no statements", rep, NothingToCompare)
	}
	if rep.UpgradeAt.IsZero() || rep.AnalyzeAt.IsZero() || rep.State != RehearsalDestroyed {
		t.Fatalf("upgrade not timed or clone not destroyed: %+v", rep)
	}
}

// Error propagation: an upgrade error destroys the clone and records the
// provider's message.
func TestRehearsalDB_UpgradeErrorDestroysTheClone(t *testing.T) {
	f := newRehearsalFixture(t)
	cause := errors.New("InvalidParameterCombination: cannot upgrade aurora-postgresql " +
		"from 14.9 to 17.4")
	rep, err := runFixture(t, f, &fixtureRehearser{err: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("Rehearse = %v, want the upgrade error", err)
	}
	if !strings.Contains(rep.ProviderMessage, "InvalidParameterCombination") {
		t.Fatalf("provider message = %q", rep.ProviderMessage)
	}
	want := []string{RehearsalRequested, RehearsalCloned, RehearsalFailed, RehearsalDestroyed}
	if got := stateNames(rep); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if got := f.inner.destroyedIDs(); len(got) != 1 || got[0] != rep.CloneID {
		t.Fatalf("destroyed %v, want the clone %q", got, rep.CloneID)
	}
	var status string
	if err := f.control.pool.QueryRow(f.control.ctx,
		"SELECT status FROM sage.clone_instances WHERE id = $1", rep.ReceiptID).
		Scan(&status); err != nil || status != clone.StatusDestroyed {
		t.Fatalf("receipt %d status %q (%v), want destroyed", rep.ReceiptID, status, err)
	}
}

// Invalid input: an upgrader that answers with a server at the wrong
// major is refused, and the clone is destroyed.
func TestRehearsalDB_UpgradedCloneAtTheWrongMajorIsRefused(t *testing.T) {
	f := newRehearsalFixture(t)
	opts := f.options(t, f.receipting(t, f.control.ctx, 0),
		&fixtureRehearser{dsn: f.sourceDSN})
	opts.TargetMajor = f.sourceMajor + 1
	rep, err := Rehearse(f.control.ctx, opts)
	if err == nil || !strings.Contains(err.Error(), "answered as PostgreSQL") {
		t.Fatalf("Rehearse = %v, want the wrong major refused", err)
	}
	if rep.State != RehearsalDestroyed || len(f.inner.destroyedIDs()) != 1 {
		t.Fatalf("state %q destroyed %v", rep.State, f.inner.destroyedIDs())
	}
}

// Crash: a rehearsal killed mid-upgrade leaves a receipt whose holder
// stops heartbeating; the reaper destroys the clone.
func TestRehearsalDB_KilledMidRehearsalTheReaperDestroysTheClone(t *testing.T) {
	f := newRehearsalFixture(t)
	lifetime, die := context.WithCancel(f.control.ctx)
	p := f.receipting(t, lifetime, 50*time.Millisecond)
	up := &fixtureRehearser{entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(f.control.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Rehearse(ctx, f.options(t, p, up))
	}()
	<-up.entered
	die() // the process is gone: no more heartbeats
	reaper := clone.Reaper{Store: f.store, Provider: f.inner, Purpose: clone.PurposeRehearsal,
		Scope: clone.ReceiptScope{DeploymentID: rehearsalDeployment, Adapter: "fixture",
			Scope: "drill-02b"}, Holder: "another-process", StaleAfter: time.Second,
		AsOf: time.Now().Add(time.Hour)}
	got, err := reaper.Reap(f.control.ctx)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(got.Destroyed) != 1 || got.Destroyed[0].Purpose != clone.PurposeRehearsal {
		t.Fatalf("reaped %+v, want the one rehearsal clone", got)
	}
	if ids := f.inner.destroyedIDs(); len(ids) == 0 || ids[0] != "fixture-1" {
		t.Fatalf("destroyed %v, want fixture-1", ids)
	}
	cancel()
	<-done
}
