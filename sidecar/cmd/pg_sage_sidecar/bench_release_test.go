package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/testdb"
	srebench "github.com/pg-sage/sidecar/sre-bench"
)

// Roadmap 1.1 (2026-10-03) wiring: the sidecar ingests the bench reports
// shipped with it (the image's /usr/share/pg_sage/bench, or bench/ next
// to the binary) at startup and hourly, with bench_results_path; it
// verifies their signatures against the embedded Sigstore root; and
// "Run bench locally" runs on a clone or the local development database,
// never on a monitored or the control database.

const releaseCommit = "cccccccccccccccccccccccccccccccccccccccc"

// asRelease runs the test as pg_sage 1.8.5 at releaseCommit.
func asRelease(t *testing.T) {
	t.Helper()
	oldVersion, oldCommit := version, commit
	version, commit = "1.8.5", releaseCommit
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })
}

// withShippedDirs points the shipped-report lookup at dirs.
func withShippedDirs(t *testing.T, dirs ...string) {
	t.Helper()
	old := shippedBenchDirs
	shippedBenchDirs = func() []string { return dirs }
	t.Cleanup(func() { shippedBenchDirs = old })
}

// releaseFakeVerifier accepts a bundle reading "valid:<report>".
type releaseFakeVerifier struct{ commit string }

func (v releaseFakeVerifier) Verify(report, bundle []byte) (benchsig.Signature, error) {
	if !bytes.Equal(bundle, append([]byte("valid:"), report...)) {
		return benchsig.Signature{}, benchsig.ErrInvalidSignature
	}
	return benchsig.Signature{Identity: "https://github.com/jasonmassie01/pg_sage/" +
		".github/workflows/ci.yml@refs/tags/v1.8.5", Issuer: benchsig.GitHubIssuer,
		Commit: v.commit, SignedAt: time.Now().UTC()}, nil
}

func withVerifier(t *testing.T, v benchingest.Verifier, err error) {
	t.Helper()
	old := newBenchVerifier
	newBenchVerifier = func() (benchingest.Verifier, error) { return v, err }
	t.Cleanup(func() { newBenchVerifier = old })
}

// writeShippedReport writes a stamped one-family report into dir/shard,
// signed with a fake bundle when signed.
func writeShippedReport(t *testing.T, dir, shard string, at time.Time, b earned.Build,
	family string, signed bool) string {
	t.Helper()
	rate := 1.0
	report := srebench.Report{Schema: srebench.ReportSchema, GeneratedAt: at,
		PgSageVersion: b.Version, PgSageCommit: b.Commit, Gated: []string{"causal-graph"},
		Cells: []srebench.CellRecord{{Arm: "causal-graph", Family: family, Runs: 12,
			SafePass: srebench.Metric{K: 12, N: 12}, Top1: srebench.Metric{K: 11, N: 12},
			MechanismPrecision: &rate}}}
	jsonPath, _, err := srebench.WriteReport(filepath.Join(dir, shard), report)
	if err != nil {
		t.Fatal(err)
	}
	if signed {
		raw, err := os.ReadFile(jsonPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jsonPath+benchingest.BundleSuffix,
			append([]byte("valid:"), raw...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return jsonPath
}

func TestAutonomyServiceConfigCarriesTheRunningBuild(t *testing.T) {
	asRelease(t)
	got := autonomyServiceConfig(config.DefaultConfig().SRE.Autonomy).Build
	if got != (earned.Build{Version: "1.8.5", Commit: releaseCommit}) {
		t.Fatalf("build = %+v", got)
	}
	version, commit = "dev", "none"
	if got := runningBuild(); got.Commit != "" || got.Version != "dev" {
		t.Fatalf("a dev build = %+v, want version dev and no commit", got)
	}
}

func TestBenchSourcesShippedFirstThenTheOperatorPath(t *testing.T) {
	withShippedDirs(t, "/usr/share/pg_sage/bench", "/opt/pg_sage/bench")
	s := config.DefaultConfig().SRE.Autonomy
	sources := benchSources(s)
	if len(sources) != 2 || !sources[0].Shipped || !sources[1].Shipped ||
		sources[0].Actor != "release_bench" {
		t.Fatalf("sources without bench_results_path = %+v", sources)
	}
	s.BenchResultsPath = "/etc/pg_sage/bench"
	sources = benchSources(s)
	last := sources[len(sources)-1]
	if len(sources) != 3 || last.Path != "/etc/pg_sage/bench" || last.Shipped ||
		last.Actor != "bench_results_path" {
		t.Fatalf("sources = %+v", sources)
	}
}

// At startup (the ingest loop runs once immediately, then hourly) the
// sidecar ingests the signed report shipped in its image, verified and
// stamped with its own build.
func TestShippedSignedReportIsIngestedAtStartup(t *testing.T) {
	asRelease(t)
	pool := autonomyPool(t)
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool, "orders",
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	image := t.TempDir()
	// In the past: the package's tests share one deployment, and a later
	// test's newer report must win (newest report per family).
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	build := earned.Build{Version: "1.8.5", Commit: releaseCommit}
	writeShippedReport(t, image, "core", at, build, "lock_blocking", true)
	writeShippedReport(t, image, "runway", at.Add(time.Second), build,
		"sequence_runway", false)
	withShippedDirs(t, image)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	benchIngester(svc, config.DefaultConfig().SRE.Autonomy)(context.Background())
	ev, err := svc.Evidence(context.Background(), earned.FamilyLockBlocking,
		earned.ClassBackendCancel)
	if err != nil || ev.Bench == nil || ev.Bench.Origin != earned.OriginSignedRelease ||
		ev.Bench.Build != build || ev.Bench.Signature == nil ||
		ev.Bench.IngestedBy != "release_bench" {
		t.Fatalf("lock_blocking bench = %+v (%v), want the signed shipped report", ev.Bench,
			err)
	}
	ev, err = svc.Evidence(context.Background(), earned.FamilySequence,
		earned.ApplicableClasses(earned.FamilySequence)[0])
	if err != nil || ev.Bench == nil || ev.Bench.Origin != earned.OriginOperator ||
		ev.Bench.Provenance != "unsigned (operator-provided)" {
		t.Fatalf("an unsigned shipped report = %+v (%v), want it unsigned", ev.Bench, err)
	}
}

func TestShippedReportForAnotherVersionIsRefused(t *testing.T) {
	asRelease(t)
	pool := autonomyPool(t)
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool, "orders",
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	image := t.TempDir()
	other := earned.Build{Version: "1.8.4",
		Commit: "dddddddddddddddddddddddddddddddddddddddd"}
	path := writeShippedReport(t, image, "core", time.Now().UTC(), other, "wal_retention",
		true)
	withVerifier(t, releaseFakeVerifier{commit: other.Commit}, nil)
	res, err := ingestBenchSources(context.Background(), svc, []benchingest.Source{
		{Path: image, Shipped: true, Actor: "release_bench"}})
	if !errors.Is(err, earned.ErrBuildMismatch) || res.Added != 0 {
		t.Fatalf("ingest = %+v (%v), want ErrBuildMismatch", res, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	run, err := earned.ParseBenchReport(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.sre_eval_runs
		WHERE encode(report_sha256, 'hex') = $1`, run.SHA256).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the refused report was stored %d times (%v)", n, err)
	}
}

// Without verification material the shipped report is still ingested,
// never as signed.
func TestShippedReportWithoutVerificationMaterialIsUnsigned(t *testing.T) {
	asRelease(t)
	withVerifier(t, nil, errors.New("embedded trusted root unreadable"))
	pool := autonomyPool(t)
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool, "orders",
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	image := t.TempDir()
	writeShippedReport(t, image, "reactive", time.Now().UTC(),
		earned.Build{Version: "1.8.5", Commit: releaseCommit}, "checkpoint_storm", true)
	res, err := ingestBenchSources(context.Background(), svc, []benchingest.Source{
		{Path: image, Shipped: true, Actor: "release_bench"}})
	if err != nil || res.Signed != 0 || res.Files != 1 {
		t.Fatalf("ingest = %+v (%v)", res, err)
	}
}

func TestLocalBenchProviderNeverTargetsAMonitoredDatabase(t *testing.T) {
	monitored := "postgres://app:pw@db.prod:5432/orders"
	control := "postgres://sage:pw@meta.internal:5432/sage_meta"
	refused := []string{monitored, control}
	none := config.CloneProviderConfig{Provider: "none"}
	s := config.DefaultConfig().SRE.Autonomy
	if p, name, err := localBenchProvider(s, none, refused); err != nil || p != nil ||
		name != "" {
		t.Fatalf("nothing configured = %v %q %v, want off", p, name, err)
	}
	for _, dsn := range []string{monitored, control,
		"postgres://other:pw@DB.PROD:5432/orders?sslmode=require"} {
		s.GameDays.LocalDSN = dsn
		if _, _, err := localBenchProvider(s, none, refused); !errors.Is(err,
			gameday.ErrMonitoredDSN) {
			t.Errorf("%s: err = %v, want ErrMonitoredDSN", dsn, err)
		}
	}
	// Game days need not be enabled for a local bench run.
	s.GameDays.Enabled = false
	s.GameDays.LocalDSN = "postgres://u:pw@127.0.0.1:5999/scratch"
	if p, name, err := localBenchProvider(s, none, refused); err != nil || p == nil ||
		name != "local" {
		t.Fatalf("disposable local database = %v %q %v", p, name, err)
	}
}

// End to end on real PostgreSQL: a local run of one family on the
// disposable database counts as bench evidence for that family only,
// marked "local run" and stamped with the running build.
func TestLocalBenchRunCountsForTheFamilyItCovered(t *testing.T) {
	asRelease(t)
	dsn := testdb.SkipUnlessLive(t)
	pool := autonomyPool(t)
	settings := config.DefaultConfig().SRE.Autonomy
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool, "orders",
		settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.GameDays.LocalDSN = dsn
	bench, err := newLocalBenchFor("orders", svc, settings,
		config.CloneProviderConfig{Provider: "none"},
		[]string{"postgres://app@db.prod:5432/orders"})
	if err != nil || bench == nil {
		t.Fatalf("local bench = %v (%v)", bench, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	run, err := bench.Run(ctx, []string{"sequence_runway"})
	if err != nil || run.Status != gameday.StatusCompleted || run.EvalRunID == "" {
		t.Fatalf("local run = %+v (%v)", run, err)
	}
	ev, err := svc.Evidence(ctx, earned.FamilySequence,
		earned.ApplicableClasses(earned.FamilySequence)[0])
	if err != nil || ev.Bench == nil || ev.Bench.ID != run.EvalRunID ||
		ev.Bench.Origin != earned.OriginLocalRun || ev.Bench.Build.Version != "1.8.5" ||
		ev.Bench.Build.Commit != releaseCommit {
		t.Fatalf("sequence_runway bench = %+v (%v)", ev.Bench, err)
	}
	for _, c := range ev.Bench.Cells {
		if c.Family != "sequence_runway" && c.Family != "all" {
			t.Fatalf("the local run scored %s, which it did not cover", c.Family)
		}
		// The run repeats until the family reaches the promotion sample.
		if c.Arm == "causal-graph" && c.Family == "sequence_runway" && c.Top1.N < 10 {
			t.Fatalf("top-1 n = %d, want >= 10 (MinTop1N)", c.Top1.N)
		}
	}
}

func TestBenchVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "pgincidentbench.json")
	if err := os.WriteFile(report, []byte(`{"pg_sage_commit":"`+releaseCommit+`"}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	writeBundle := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(report+benchingest.BundleSuffix, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := runBenchCommand(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, stderr := run(); code != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("no args = %d %q", code, stderr)
	}
	if code, _, _ := run("verify"); code != 2 {
		t.Errorf("verify without a report = %d", code)
	}
	if code, _, _ := run("rebuild", report); code != 2 {
		t.Errorf("unknown subcommand = %d", code)
	}
	if code, _, stderr := run("verify", report); code != 1 ||
		!strings.Contains(stderr, "sigstore.json") {
		t.Errorf("missing bundle = %d %q", code, stderr)
	}
	writeBundle(append([]byte("valid:"), raw...))
	code, stdout, stderr := run("verify", report)
	if code != 0 || !strings.Contains(stdout, "ci.yml@refs/tags/v1.8.5") ||
		!strings.Contains(stdout, releaseCommit[:7]) {
		t.Errorf("valid = %d %q %q", code, stdout, stderr)
	}
	if code, _, _ := run("verify", "--commit", releaseCommit, report); code != 0 {
		t.Errorf("matching --commit = %d", code)
	}
	if code, _, stderr := run("verify", "--commit", strings.Repeat("e", 40), report); code != 1 ||
		!strings.Contains(stderr, "commit") {
		t.Errorf("other --commit = %d %q", code, stderr)
	}
	writeBundle([]byte("valid:something else"))
	if code, _, stderr := run("verify", report); code != 1 ||
		!strings.Contains(stderr, "signature") {
		t.Errorf("tampered = %d %q", code, stderr)
	}
	withVerifier(t, nil, fmt.Errorf("no trusted root"))
	if code, _, stderr := run("verify", report); code != 1 ||
		!strings.Contains(stderr, "trusted root") {
		t.Errorf("no verifier = %d %q", code, stderr)
	}
}

// operatorIngest ingests path the way sre.autonomy.bench_results_path is
// ingested and counts the new reports.
func operatorIngest(ctx context.Context, svc *earned.Service, path string) (int, error) {
	res, err := ingestBenchSources(ctx, svc, []benchingest.Source{{Path: path,
		Actor: "bench_results_path"}})
	return res.Added, err
}
