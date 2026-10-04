package earned

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Roadmap 2.4 against real PostgreSQL: the model lift travels with the
// ingested report (same provenance, same build matching as promotion
// evidence), the newest measurement of a family decides its root
// authority, and a replay-only (lift-only) report never displaces the
// report promotions read.

func TestModelLift_SignedReportGrantsOnlyItsMeasuredFamily(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := stampLift(liftReport(fixtureEpoch.Add(-time.Hour), "live", false,
		good("lock_blocking", 16, 16), good("wal_retention", 15, 15)), buildA)
	run, err := f.ingestSigned(raw, commitA)
	if err != nil {
		t.Fatalf("signed lift report refused: %v", err)
	}
	if run.ModelLift == nil || len(run.ModelLift.Records) != 2 {
		t.Fatalf("ingested run lift = %+v", run.ModelLift)
	}
	lock, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || !lock.Granted || lock.Report == nil || !lock.Report.Signed ||
		lock.Report.ID != run.ID {
		t.Fatalf("lock_blocking = %+v (%v)", lock, err)
	}
	wal, err := f.svc.ModelRootAuthority(f.ctx, FamilyWAL)
	if err != nil || wal.Granted || !strings.Contains(wal.Reason, "lower bound") {
		t.Fatalf("wal_retention = %+v (%v)", wal, err)
	}
	conn, err := f.svc.ModelRootAuthority(f.ctx, FamilyConnections)
	if err != nil || conn.Granted || conn.Report != nil {
		t.Fatalf("connection_pressure = %+v (%v)", conn, err)
	}
}

func TestModelLift_NewestMeasurementWins(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	older := stampLift(liftReport(fixtureEpoch.Add(-3*time.Hour), "live", false,
		good("lock_blocking", 40, 40)), buildA)
	newer := stampLift(liftReport(fixtureEpoch.Add(-time.Hour), "live", false,
		good("lock_blocking", 2, 20)), buildA)
	for _, raw := range [][]byte{newer, older} { // ingest order must not matter
		if _, err := f.ingestSigned(raw, commitA); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || got.Granted || got.Lift == nil || got.Lift.Overrides != (Metric{2, 20}) {
		t.Fatalf("authority = %+v (%v)", got, err)
	}
}

func TestModelLift_ReportForAnotherBuildNeverCounts(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	other := Build{Version: "9.9.9", Commit: strings.Repeat("e", 40)}
	raw := stampLift(liftReport(fixtureEpoch.Add(-time.Hour), "live", false,
		good("lock_blocking", 40, 40)), other)
	if _, err := f.ingestSigned(raw, other.Commit); err == nil {
		t.Fatal("a lift report for another build was accepted")
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || got.Granted {
		t.Fatalf("authority = %+v (%v)", got, err)
	}
}

func TestModelLift_V190ReportsStayReadable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.IngestEvalRun(f.ctx,
		benchReport(fixtureEpoch.Add(-time.Hour), FamilyLockBlocking), SourceBench, "admin",
		""); err != nil {
		t.Fatalf("ingest a v1.9.0 report: %v", err)
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || got.Granted || !strings.Contains(got.Reason, "no held-out") {
		t.Fatalf("authority without any lift = %+v (%v)", got, err)
	}
	latest, err := f.store.LatestBench(f.ctx, FamilyLockBlocking)
	if err != nil || latest == nil || latest.ModelLift != nil {
		t.Fatalf("latest v1.9.0 report = %+v (%v)", latest, err)
	}
}

func TestModelLift_LiftOnlyReportDoesNotDisplacePromotionEvidence(t *testing.T) {
	f := newFixture(t)
	release, err := f.svc.IngestEvalRun(f.ctx,
		benchReport(fixtureEpoch.Add(-2*time.Hour), FamilyLockBlocking), SourceBench, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.IngestEvalRun(f.ctx,
		liftReport(fixtureEpoch.Add(-time.Hour), "live", false,
			good("lock_blocking", 16, 16)), SourceBench, "admin", ""); err != nil {
		t.Fatalf("ingest a lift-only report: %v", err)
	}
	v, err := f.svc.View(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Bench == nil || v.Bench.ID != release.ID {
		t.Fatalf("view bench = %+v, want the release report %s", v.Bench, release.ID)
	}
	for _, fv := range v.Families {
		if fv.Family == FamilyLockBlocking && (fv.Bench == nil || fv.Bench.ID != release.ID) {
			t.Fatalf("lock_blocking bench = %+v, want %s", fv.Bench, release.ID)
		}
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || !got.Granted || got.Report == nil || got.Report.Signed ||
		!strings.Contains(got.Report.Provenance, "unsigned") {
		t.Fatalf("an operator lift report counts like operator promotion evidence, "+
			"labelled unsigned: %+v (%v)", got, err)
	}
}

func TestModelLift_ViewListsEveryFamily(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.IngestEvalRun(f.ctx,
		liftReport(fixtureEpoch.Add(-time.Hour), "live", false,
			good("lock_blocking", 16, 16)), SourceBench, "admin", ""); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.ModelLiftView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Families) != len(Families()) || view.Threshold != 0.80 ||
		view.MinOverrides != 10 || view.Meaning == "" {
		t.Fatalf("view = %+v", view)
	}
	seen := map[Family]RootAuthority{}
	for _, a := range view.Families {
		seen[a.Family] = a
	}
	if a := seen[FamilyLockBlocking]; !a.Granted || a.Lift == nil {
		t.Fatalf("lock_blocking = %+v", a)
	}
	if a := seen[FamilyWAL]; a.Granted || a.Lift != nil || a.Status != RootAdvisory {
		t.Fatalf("wal_retention = %+v", a)
	}
}

func TestModelLift_ConcurrentDuplicateIngestStoresOnce(t *testing.T) {
	f := newFixture(t)
	raw := liftReport(fixtureEpoch.Add(-time.Hour), "live", false, good("lock_blocking", 16, 16))
	var wg sync.WaitGroup
	ids := make([]string, 4)
	errs := make([]error, 4)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run, err := f.svc.IngestEvalRun(f.ctx, raw, SourceBench, "admin", "")
			ids[i], errs[i] = run.ID, err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || ids[i] != ids[0] {
			t.Fatalf("ingest %d: id %s vs %s (%v)", i, ids[i], ids[0], err)
		}
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.sre_eval_runs
		WHERE deployment_id = $1 AND model_lift IS NOT NULL`,
		f.store.DeploymentID()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored lift reports = %d (%v)", n, err)
	}
}

// stampLift names the build a lift report scored.
func stampLift(raw []byte, b Build) []byte {
	return []byte(strings.Replace(string(raw), `"generated_at"`,
		`"pg_sage_version": "`+b.Version+`", "pg_sage_commit": "`+b.Commit+
			`", "generated_at"`, 1))
}

// A lift-only report may carry no cells at all; it is stored with an
// empty cell list, and a row whose cells are not an array (a JSON null
// from an older writer) never breaks the newest-report query.
func TestModelLift_ReportWithoutCellsKeepsTheLedgerReadable(t *testing.T) {
	f := newFixture(t)
	noCells := strings.Replace(string(liftReport(fixtureEpoch.Add(-time.Hour), "live", false,
		good("lock_blocking", 16, 16))), `"cells": [{`, `"cells": [], "x": [{`, 1)
	run, err := f.svc.IngestEvalRun(f.ctx, []byte(noCells), SourceBench, "admin", "")
	if err != nil || run.Cells == nil {
		t.Fatalf("ingest a report without cells: %+v (%v)", run.Cells, err)
	}
	var kind string
	if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_typeof(cells) FROM sage.sre_eval_runs
		WHERE deployment_id = $1 AND id = $2`, f.store.DeploymentID(), run.ID).Scan(
		&kind); err != nil || kind != "array" {
		t.Fatalf("stored cells type %q (%v)", kind, err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.sre_eval_runs SET cells = 'null'
		WHERE deployment_id = $1 AND id = $2`, f.store.DeploymentID(), run.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := f.svc.View(f.ctx); err != nil || v.Bench != nil {
		t.Fatalf("view over a report with scalar cells = %+v (%v)", v.Bench, err)
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || !got.Granted {
		t.Fatalf("authority = %+v (%v)", got, err)
	}
}
