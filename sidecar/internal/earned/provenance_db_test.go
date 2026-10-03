package earned

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Roadmap 1.1 against real PostgreSQL: provenance is stored with the
// report and shown with the evidence; a report for another build never
// counts; a local run counts only for the families it covered.

const ciIdentity = "https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml" +
	"@refs/tags/v1.8.5"

// newBuildFixture is newFixtureFor with the running pg_sage build set.
func newBuildFixture(t *testing.T, deployment string, build Build) *fixture {
	t.Helper()
	f := newFixtureFor(t, deployment, "orders")
	f.svc.cfg.Build = build
	return f
}

// stampedReport is benchReport naming the pg_sage build it scored.
func stampedReport(at time.Time, b Build, families ...Family) []byte {
	raw := string(benchReport(at, families...))
	stamp := fmt.Sprintf(`"pg_sage_version": %q, "pg_sage_commit": %q, "generated_at"`,
		b.Version, b.Commit)
	return []byte(strings.Replace(raw, `"generated_at"`, stamp, 1))
}

func signatureFor(commit string) *ReportSignature {
	return &ReportSignature{Identity: ciIdentity,
		Issuer: "https://token.actions.githubusercontent.com", Commit: commit,
		SignedAt: fixtureEpoch.Add(-2 * time.Hour)}
}

func (f *fixture) ingestSigned(raw []byte, commit string) (EvalRun, error) {
	return f.svc.IngestBench(f.ctx, raw, BenchIngest{Origin: OriginSignedRelease,
		Actor: "release_bench", Signature: signatureFor(commit), RequireBuild: true})
}

func TestIngestBenchStoresASignedReleaseReport(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := stampedReport(fixtureEpoch.Add(-time.Hour), buildA, FamilyLockBlocking)
	run, err := f.ingestSigned(raw, commitA)
	if err != nil {
		t.Fatalf("signed report refused: %v", err)
	}
	if run.Origin != OriginSignedRelease || run.Build != buildA || run.Signature == nil ||
		run.Signature.Identity != ciIdentity || run.Duplicate {
		t.Fatalf("run = %+v", run)
	}
	got, err := f.store.LatestBench(f.ctx, FamilyLockBlocking)
	if err != nil || got == nil || got.ID != run.ID {
		t.Fatalf("latest = %+v (%v)", got, err)
	}
	if got.Origin != OriginSignedRelease || got.Build != buildA || got.Signature == nil ||
		got.Signature.Commit != commitA || !got.Signature.SignedAt.Equal(
		signatureFor(commitA).SignedAt) || !strings.Contains(got.Provenance, "signed") {
		t.Fatalf("stored provenance = %+v", got)
	}
	v, err := f.svc.View(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, fv := range v.Families {
		switch {
		case fv.Family == FamilyLockBlocking && (fv.Bench == nil || !fv.Bench.Signed ||
			fv.Bench.ID != run.ID || !strings.Contains(fv.Bench.Provenance, "1.8.5")):
			t.Fatalf("lock_blocking bench summary = %+v", fv.Bench)
		case fv.Family == FamilyWAL && fv.Bench != nil:
			t.Fatalf("wal_retention has a bench summary from a report without it: %+v",
				fv.Bench)
		}
	}
	if v.Bench == nil || v.Bench.Origin != OriginSignedRelease {
		t.Fatalf("view bench = %+v", v.Bench)
	}
}

func TestIngestBenchRefusesAReportForAnotherBuild(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	at := fixtureEpoch.Add(-time.Hour)
	for name, raw := range map[string][]byte{
		"another release":     stampedReport(at, buildB, FamilyLockBlocking),
		"a rebuilt tag":       stampedReport(at, Build{"1.8.5", commitB}, FamilyLockBlocking),
		"a version, no build": stampedReport(at, Build{Version: "1.8.4"}, FamilyLockBlocking),
	} {
		for _, in := range []BenchIngest{
			{Origin: OriginOperator, Actor: "user:1:admin@example.com"},
			{Origin: OriginSignedRelease, Actor: "release_bench",
				Signature: signatureFor(""), RequireBuild: true},
		} {
			_, err := f.svc.IngestBench(f.ctx, raw, in)
			if !errors.Is(err, ErrBuildMismatch) || !errors.Is(err, ErrInvalidReport) {
				t.Errorf("%s via %s: err = %v, want ErrBuildMismatch", name, in.Origin, err)
			}
		}
	}
	if got, err := f.store.LatestBench(f.ctx, ""); err != nil || got != nil {
		t.Fatalf("a refused report was stored: %+v (%v)", got, err)
	}
	// The same commit under another version label is the same code.
	if _, err := f.svc.IngestBench(f.ctx, stampedReport(at, Build{"1.8.4", commitA},
		FamilyLockBlocking), BenchIngest{Origin: OriginOperator, Actor: "ops"}); err != nil {
		t.Fatalf("same commit refused: %v", err)
	}
}

func TestIngestBenchSignatureMustCoverTheReportCommit(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := stampedReport(fixtureEpoch.Add(-time.Hour), buildA, FamilyLockBlocking)
	_, err := f.ingestSigned(raw, commitB)
	if !errors.Is(err, ErrSignatureMismatch) || errors.Is(err, ErrBuildMismatch) {
		t.Fatalf("err = %v, want ErrSignatureMismatch (distinguishable)", err)
	}
	// A certificate without a source digest binds the content only.
	if run, err := f.ingestSigned(raw, ""); err != nil || run.Origin != OriginSignedRelease {
		t.Fatalf("signature without a digest = %+v (%v)", run, err)
	}
}

func TestIngestBenchOriginRules(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := stampedReport(fixtureEpoch.Add(-time.Hour), buildA, FamilyLockBlocking)
	for name, in := range map[string]BenchIngest{
		"signed without a signature": {Origin: OriginSignedRelease, Actor: "release_bench"},
		"operator with a signature": {Origin: OriginOperator, Actor: "ops",
			Signature: signatureFor(commitA)},
		"local run with a signature": {Origin: OriginLocalRun, Actor: ActorPgSage,
			Signature: signatureFor(commitA)},
		"a game day as bench":   {Origin: OriginGameDay, Actor: ActorPgSage},
		"an unknown origin":     {Origin: "trusted", Actor: "ops"},
		"no origin":             {Actor: "ops"},
		"no actor":              {Origin: OriginOperator},
		"an oversized actor":    {Origin: OriginOperator, Actor: strings.Repeat("a", 201)},
		"a signature, no actor": {Origin: OriginSignedRelease, Signature: signatureFor(commitA)},
	} {
		if _, err := f.svc.IngestBench(f.ctx, raw, in); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := f.svc.IngestBench(f.ctx, []byte("{"), BenchIngest{Origin: OriginOperator,
		Actor: "ops"}); !errors.Is(err, ErrInvalidReport) {
		t.Errorf("malformed report: err = %v", err)
	}
}

// Shipped, signed and local-run reports must name the build: only an
// operator report may be unstamped (and then works as before).
func TestIngestBenchRequiresTheBuildWhereProvenanceClaimsIt(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := benchReport(fixtureEpoch.Add(-time.Hour), FamilyLockBlocking)
	for name, in := range map[string]BenchIngest{
		"shipped unsigned": {Origin: OriginOperator, Actor: "release_bench",
			RequireBuild: true},
		"signed":    {Origin: OriginSignedRelease, Actor: "release_bench", Signature: signatureFor("")},
		"local run": {Origin: OriginLocalRun, Actor: ActorPgSage},
	} {
		if _, err := f.svc.IngestBench(f.ctx, raw, in); !errors.Is(err, ErrBuildMismatch) {
			t.Errorf("%s: err = %v, want ErrBuildMismatch", name, err)
		}
	}
}

func TestUnsignedOperatorReportWorksAsBefore(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	run, err := f.svc.IngestEvalRun(f.ctx, benchReport(fixtureEpoch.Add(-time.Hour),
		FamilyLockBlocking), SourceBench, "user:1:admin@example.com", "")
	if err != nil {
		t.Fatalf("unsigned upload refused: %v", err)
	}
	if run.Origin != OriginOperator || run.Signature != nil || run.Build.Known() ||
		run.Provenance != "unsigned (operator-provided)" {
		t.Fatalf("run = %+v", run)
	}
	ev, err := f.svc.Evidence(f.ctx, FamilyLockBlocking, ClassBackendCancel)
	if err != nil || ev.Bench == nil || ev.Bench.ID != run.ID ||
		ev.Bench.Provenance != "unsigned (operator-provided)" {
		t.Fatalf("evidence bench = %+v (%v)", ev.Bench, err)
	}
	a := Assess(f.svc.cfg.Thresholds, L2, ev)
	for _, c := range a.Checks {
		if c.Name == "bench_present" && !c.Met {
			t.Fatalf("an unsigned operator report no longer counts: %+v", c)
		}
	}
}

func TestLocalRunCountsOnlyForTheFamiliesItCovered(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	signed, err := f.ingestSigned(stampedReport(fixtureEpoch.Add(-2*time.Hour), buildA,
		FamilyLockBlocking, FamilyWAL), commitA)
	if err != nil {
		t.Fatal(err)
	}
	local, err := f.svc.IngestBench(f.ctx, stampedReport(fixtureEpoch.Add(-time.Hour),
		buildA, FamilyLockBlocking), BenchIngest{Origin: OriginLocalRun, Actor: ActorPgSage})
	if err != nil {
		t.Fatalf("local run refused: %v", err)
	}
	if local.Origin != OriginLocalRun || !strings.Contains(local.Provenance, "local run") {
		t.Fatalf("local run = %+v", local)
	}
	for family, want := range map[Family]string{FamilyLockBlocking: local.ID,
		FamilyWAL: signed.ID} {
		ev, err := f.svc.Evidence(f.ctx, family, ApplicableClasses(family)[0])
		if err != nil || ev.Bench == nil || ev.Bench.ID != want {
			t.Errorf("%s bench = %+v (%v), want %s", family, ev.Bench, err, want)
		}
	}
}

func TestReportsOfAnotherBuildStopCountingAfterAnUpgrade(t *testing.T) {
	deployment := newUUID(t)
	old := newBuildFixture(t, deployment, buildA)
	if _, err := old.ingestSigned(stampedReport(fixtureEpoch.Add(-2*time.Hour), buildA,
		FamilyLockBlocking), commitA); err != nil {
		t.Fatal(err)
	}
	legacy, err := old.svc.IngestEvalRun(old.ctx, benchReport(fixtureEpoch.Add(-3*time.Hour),
		FamilyWAL), SourceBench, "user:1:admin@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	upgraded := newBuildFixture(t, deployment, buildB)
	ev, err := upgraded.svc.Evidence(upgraded.ctx, FamilyLockBlocking, ClassBackendCancel)
	if err != nil || ev.Bench != nil {
		t.Fatalf("a report for %s counts for %s: %+v (%v)", buildA.Version, buildB.Version,
			ev.Bench, err)
	}
	// An unstamped operator report names no build and keeps counting.
	ev, err = upgraded.svc.Evidence(upgraded.ctx, FamilyWAL, ApplicableClasses(FamilyWAL)[0])
	if err != nil || ev.Bench == nil || ev.Bench.ID != legacy.ID {
		t.Fatalf("legacy report = %+v (%v)", ev.Bench, err)
	}
	if latest, err := upgraded.store.LatestBench(upgraded.ctx, ""); err != nil ||
		latest == nil || latest.ID != legacy.ID {
		t.Fatalf("latest overall = %+v (%v), want the legacy report", latest, err)
	}
}

func TestDuplicateSignedReportUpgradesProvenance(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := stampedReport(fixtureEpoch.Add(-time.Hour), buildA, FamilyLockBlocking)
	first, err := f.svc.IngestBench(f.ctx, raw, BenchIngest{Origin: OriginOperator,
		Actor: "bench_results_path"})
	if err != nil || first.Origin != OriginOperator {
		t.Fatalf("operator copy = %+v (%v)", first, err)
	}
	second, err := f.ingestSigned(raw, commitA)
	if err != nil || !second.Duplicate || second.ID != first.ID ||
		second.Origin != OriginSignedRelease || second.Signature == nil {
		t.Fatalf("signed copy = %+v (%v), want the same run now signed", second, err)
	}
	third, err := f.svc.IngestBench(f.ctx, raw, BenchIngest{Origin: OriginOperator,
		Actor: "bench_results_path"})
	if err != nil || !third.Duplicate || third.Origin != OriginSignedRelease {
		t.Fatalf("an unsigned copy downgraded the provenance: %+v (%v)", third, err)
	}
	got, err := f.store.LatestBench(f.ctx, FamilyLockBlocking)
	if err != nil || got == nil || got.Origin != OriginSignedRelease {
		t.Fatalf("stored = %+v (%v)", got, err)
	}
}

func TestConcurrentIngestOfOneSignedReport(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	raw := stampedReport(fixtureEpoch.Add(-time.Hour), buildA, FamilyLockBlocking)
	var wg sync.WaitGroup
	runs := make([]EvalRun, 8)
	errs := make([]error, len(runs))
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runs[i], errs[i] = f.ingestSigned(raw, commitA)
		}(i)
	}
	wg.Wait()
	fresh := 0
	for i, run := range runs {
		if errs[i] != nil {
			t.Fatalf("ingest %d: %v", i, errs[i])
		}
		if run.ID != runs[0].ID || run.Origin != OriginSignedRelease {
			t.Fatalf("ingest %d = %+v, want run %s", i, run, runs[0].ID)
		}
		if !run.Duplicate {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d ingests stored the report, want exactly 1", fresh)
	}
}

func TestGameDayReportsAreMarkedGameDay(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	run, err := f.svc.IngestEvalRun(f.ctx, benchReport(fixtureEpoch.Add(-time.Hour),
		FamilyLockBlocking), SourceGameDay, ActorPgSage, "orders")
	if err != nil || run.Origin != OriginGameDay {
		t.Fatalf("game day = %+v (%v)", run, err)
	}
	runs, err := f.store.GameDayRuns(f.ctx, fixtureEpoch.Add(-24*time.Hour))
	if err != nil || len(runs) != 1 || runs[0].Origin != OriginGameDay ||
		runs[0].Provenance != "game day" {
		t.Fatalf("game days = %+v (%v)", runs, err)
	}
	// A game day is never bench evidence, whatever its provenance.
	if got, err := f.store.LatestBench(f.ctx, ""); err != nil || got != nil {
		t.Fatalf("game day counted as bench: %+v (%v)", got, err)
	}
}
