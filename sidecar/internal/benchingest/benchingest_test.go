package benchingest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Roadmap 1.1 (2026-10-03): bench reports reach the ledger from two
// kinds of places. Shipped reports (in the image at ImageDir, or in a
// bench/ directory next to the binary) are ingested at startup and
// hourly; each must name the running build. sre.autonomy.bench_results_path
// is the operator's. Either way a report with a "<report>.sigstore.json"
// bundle next to it is verified: a valid signature makes it a signed
// release report, an invalid one refuses it, and without a bundle it is
// an unsigned operator-provided report.
//
// No concurrent-access test here: Ingest keeps no state of its own; the
// ledger serializes stored reports (earned
// TestConcurrentIngestOfOneSignedReport).

type ledgerCall struct {
	raw []byte
	in  earned.BenchIngest
}

type fakeLedger struct {
	mu    sync.Mutex
	calls []ledgerCall
	seen  map[string]bool
	err   error
}

func (l *fakeLedger) IngestBench(_ context.Context, raw []byte,
	in earned.BenchIngest) (earned.EvalRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, ledgerCall{raw: append([]byte{}, raw...), in: in})
	if l.err != nil {
		return earned.EvalRun{}, l.err
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	dup := l.seen[string(raw)]
	l.seen[string(raw)] = true
	return earned.EvalRun{ID: "run", Origin: in.Origin, Duplicate: dup}, nil
}

// fakeVerifier accepts a bundle whose content is "valid:<report>".
type fakeVerifier struct{ calls int }

func (v *fakeVerifier) Verify(report, bundle []byte) (benchsig.Signature, error) {
	v.calls++
	if !bytes.Equal(bundle, append([]byte("valid:"), report...)) {
		return benchsig.Signature{}, benchsig.ErrInvalidSignature
	}
	return benchsig.Signature{Identity: "ci", Issuer: "gh", Commit: "abc1234",
		SignedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}, nil
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// shippedTree is the image layout: one report per CI shard, signed.
func shippedTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, shard := range []string{"core", "reactive", "runway"} {
		report := filepath.Join(root, shard, ReportName)
		write(t, report, shard+"-report")
		write(t, report+BundleSuffix, "valid:"+shard+"-report")
		write(t, filepath.Join(root, shard, "pgincidentbench.md"), "# summary")
	}
	write(t, filepath.Join(root, "README.md"), "shipped bench reports")
	return root
}

func TestIngestShippedSignedReports(t *testing.T) {
	ledger, verifier := &fakeLedger{}, &fakeVerifier{}
	root := shippedTree(t)
	res, err := Ingest(context.Background(), ledger, verifier,
		Source{Path: root, Shipped: true, Actor: "release_bench"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Files != 3 || res.Added != 3 || res.Signed != 3 || verifier.calls != 3 {
		t.Fatalf("result = %+v, verifier calls %d", res, verifier.calls)
	}
	for _, c := range ledger.calls {
		in := c.in
		if in.Origin != earned.OriginSignedRelease || in.Signature == nil ||
			in.Signature.Commit != "abc1234" || in.Signature.Identity != "ci" ||
			!in.RequireBuild || in.Actor != "release_bench" {
			t.Fatalf("ingest of %q = %+v", c.raw, in)
		}
		if strings.HasPrefix(string(c.raw), "valid:") {
			t.Fatalf("a signature bundle was ingested as a report: %q", c.raw)
		}
	}
	// Hourly re-ingest: the same reports again, nothing new.
	res, err = Ingest(context.Background(), ledger, verifier,
		Source{Path: root, Shipped: true, Actor: "release_bench"})
	if err != nil || res.Files != 3 || res.Added != 0 || res.Signed != 3 {
		t.Fatalf("re-ingest = %+v (%v), want 3 signed duplicates", res, err)
	}
}

func TestIngestRefusesATamperedSignedReport(t *testing.T) {
	root := shippedTree(t)
	write(t, filepath.Join(root, "core", ReportName), "core-report, edited")
	ledger := &fakeLedger{}
	res, err := Ingest(context.Background(), ledger, &fakeVerifier{},
		Source{Path: root, Shipped: true, Actor: "release_bench"})
	if !errors.Is(err, benchsig.ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
	if !strings.Contains(err.Error(), "core") {
		t.Fatalf("the error does not name the report: %v", err)
	}
	// The other shards still go in; the tampered one never does.
	if res.Added != 2 || len(ledger.calls) != 2 {
		t.Fatalf("result = %+v, %d ledger calls", res, len(ledger.calls))
	}
	for _, c := range ledger.calls {
		if strings.Contains(string(c.raw), "edited") {
			t.Fatal("a tampered report reached the ledger")
		}
	}
}

func TestIngestWithoutABundleIsUnsignedOperatorProvided(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "pgincidentbench.json"), "report")
	ledger, verifier := &fakeLedger{}, &fakeVerifier{}
	res, err := Ingest(context.Background(), ledger, verifier,
		Source{Path: dir, Actor: "bench_results_path"})
	if err != nil || res.Added != 1 || res.Signed != 0 || verifier.calls != 0 {
		t.Fatalf("result = %+v (%v), verifier calls %d", res, err, verifier.calls)
	}
	in := ledger.calls[0].in
	if in.Origin != earned.OriginOperator || in.Signature != nil || in.RequireBuild ||
		in.Actor != "bench_results_path" {
		t.Fatalf("ingest = %+v", in)
	}
}

// Without verification material (no verifier) a bundle cannot be
// checked: the report goes in unsigned, never as signed.
func TestIngestWithoutAVerifierNeverClaimsASignature(t *testing.T) {
	ledger := &fakeLedger{}
	res, err := Ingest(context.Background(), ledger, nil,
		Source{Path: shippedTree(t), Shipped: true, Actor: "release_bench"})
	if err != nil || res.Signed != 0 || res.Added != 3 {
		t.Fatalf("result = %+v (%v)", res, err)
	}
	for _, c := range ledger.calls {
		if c.in.Origin != earned.OriginOperator || c.in.Signature != nil ||
			!c.in.RequireBuild {
			t.Fatalf("ingest = %+v", c.in)
		}
	}
}

func TestIngestMissingPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	res, err := Ingest(context.Background(), &fakeLedger{}, nil,
		Source{Path: missing, Shipped: true, Actor: "release_bench"})
	if err != nil || res != (Result{}) {
		t.Fatalf("a missing shipped directory = %+v (%v), want nothing and no error", res,
			err)
	}
	if _, err := Ingest(context.Background(), &fakeLedger{}, nil,
		Source{Path: missing, Actor: "bench_results_path"}); err == nil {
		t.Fatal("a missing bench_results_path was not reported")
	}
	if _, err := Ingest(context.Background(), &fakeLedger{}, nil,
		Source{Path: "", Actor: "x"}); err == nil {
		t.Fatal("an empty path was accepted")
	}
	if _, err := Ingest(context.Background(), nil, nil,
		Source{Path: t.TempDir(), Actor: "x"}); err == nil {
		t.Fatal("a nil ledger was accepted")
	}
}

func TestIngestASingleFileAndLedgerErrors(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	write(t, report, "report")
	write(t, report+BundleSuffix, "valid:report")
	ledger := &fakeLedger{}
	res, err := Ingest(context.Background(), ledger, &fakeVerifier{},
		Source{Path: report, Actor: "bench_results_path"})
	if err != nil || res.Signed != 1 || ledger.calls[0].in.Origin != earned.OriginSignedRelease {
		t.Fatalf("single signed file = %+v (%v)", res, err)
	}
	ledger.err = earned.ErrBuildMismatch
	_, err = Ingest(context.Background(), ledger, &fakeVerifier{},
		Source{Path: report, Actor: "bench_results_path"})
	if !errors.Is(err, earned.ErrBuildMismatch) || !strings.Contains(err.Error(),
		"report.json") {
		t.Fatalf("ledger refusal = %v, want ErrBuildMismatch naming the file", err)
	}
}

func TestIngestRefusesOversizedReports(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", earned.MaxReportBytes+1)
	write(t, filepath.Join(dir, "big.json"), big)
	ledger := &fakeLedger{}
	_, err := Ingest(context.Background(), ledger, nil, Source{Path: dir, Actor: "x"})
	if err == nil || len(ledger.calls) != 0 {
		t.Fatalf("oversized report: %v, %d ledger calls", err, len(ledger.calls))
	}
}

func TestReportFilesSkipsBundlesAndDeepDirectories(t *testing.T) {
	root := shippedTree(t)
	// Directories up to MaxDepth (3) levels below the root are searched.
	write(t, filepath.Join(root, "a", "b", "c", "d", "too-deep.json"), "x")
	write(t, filepath.Join(root, "a", "b", "c", "ok.json"), "x")
	files, err := ReportFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, f := range files {
		r, _ := filepath.Rel(root, f)
		rel = append(rel, filepath.ToSlash(r))
	}
	want := "a/b/c/ok.json,core/pgincidentbench.json,reactive/pgincidentbench.json," +
		"runway/pgincidentbench.json"
	if strings.Join(rel, ",") != want {
		t.Fatalf("files = %v, want %s", rel, want)
	}
	if _, err := ReportFiles(filepath.Join(root, "absent")); err == nil {
		t.Fatal("a missing root was not reported")
	}
}

func TestShippedDirs(t *testing.T) {
	exe := filepath.Join("opt", "pg_sage", "pg_sage")
	dirs := ShippedDirs(exe)
	if len(dirs) != 2 || dirs[0] != ImageDir ||
		dirs[1] != filepath.Join("opt", "pg_sage", "bench") {
		t.Fatalf("dirs = %v", dirs)
	}
	if dirs := ShippedDirs(""); len(dirs) != 1 || dirs[0] != ImageDir {
		t.Fatalf("unknown executable: dirs = %v", dirs)
	}
	if ImageDir != "/usr/share/pg_sage/bench" {
		t.Fatalf("ImageDir = %q: the Dockerfile copies the reports there", ImageDir)
	}
}
