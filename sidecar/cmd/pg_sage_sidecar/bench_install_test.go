package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// benches-09: "pg_sage bench install" verifies a signed release report
// (the release's pgincidentbench-live.json and its .sigstore.json) for the
// running commit and copies both atomically into
// sre.autonomy.bench_results_path, where the hourly ingester stores the
// report as a signed release report.
//
// The signature is the package's releaseFakeVerifier (a bundle reading
// "valid:<report>"): the virtual Sigstore of the benchsig tests signs
// entities, not bundle files, so the real cryptography is covered there
// (benchsig_test.go) and this file covers the command around it.

// installConfig writes a config.yaml whose bench_results_path is dest
// (none when dest is empty) and returns its path.
func installConfig(t *testing.T, dest string) string {
	t.Helper()
	body := "mode: standalone\n"
	if dest != "" {
		body += "sre:\n  autonomy:\n    bench_results_path: '" + dest + "'\n"
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noEnv is a getenv that knows no variables.
func noEnv(string) string { return "" }

// signedRelease writes a signed one-family release report for build and
// returns its path.
func signedRelease(t *testing.T, b earned.Build) string {
	t.Helper()
	return writeShippedReport(t, t.TempDir(), "live", time.Now().UTC().Add(-time.Minute),
		b, "lock_blocking", true)
}

func runInstall(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runBenchInstall(args, noEnv, &out, &errb)
	return code, out.String(), errb.String()
}

// assertNothingInstalled fails when dest exists.
func assertNothingInstalled(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bench_results_path %s was written (stat: %v), want nothing copied", dest, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBenchInstallCopiesTheReportAndItsBundle(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	dest := filepath.Join(t.TempDir(), "bench")
	code, out, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 0 {
		t.Fatalf("install = %d, stderr %q", code, errText)
	}
	target := filepath.Join(dest, filepath.Base(src))
	if !bytes.Equal(readFile(t, target), readFile(t, src)) ||
		!bytes.Equal(readFile(t, target+benchingest.BundleSuffix),
			readFile(t, src+benchingest.BundleSuffix)) {
		t.Fatal("the installed report or bundle differs from the release asset")
	}
	if !strings.Contains(out, target) || !strings.Contains(out, "signed by") ||
		!strings.Contains(out, releaseCommit) {
		t.Fatalf("stdout %q must name the installed file, the signer and the commit", out)
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 2 {
		t.Fatalf("bench_results_path holds %v (%v), want the report and its bundle only",
			entries, err)
	}
}

// One ingest cycle after the install stores the report as signed_release.
func TestBenchInstallThenOneIngestStoresItSigned(t *testing.T) {
	asRelease(t)
	pool := autonomyPool(t)
	s := config.DefaultConfig().SRE.Autonomy
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool, "orders", s)
	if err != nil {
		t.Fatal(err)
	}
	withShippedDirs(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	dest := filepath.Join(t.TempDir(), "bench")
	if code, _, errText := runInstall(t, "--config", installConfig(t, dest), src); code != 0 {
		t.Fatalf("install = %d, stderr %q", code, errText)
	}
	s.BenchResultsPath = dest
	res, err := ingestBenchSources(context.Background(), svc, benchSources(s))
	if err != nil || res.Files != 1 || res.Signed != 1 || len(res.Refused) != 0 {
		t.Fatalf("ingest = %+v (%v), want one signed report", res, err)
	}
	run, err := earned.ParseBenchReport(readFile(t, src), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var origin, by string
	if err := pool.QueryRow(context.Background(), `SELECT origin, ingested_by
		FROM sage.sre_eval_runs WHERE encode(report_sha256, 'hex') = $1`,
		run.SHA256).Scan(&origin, &by); err != nil {
		t.Fatalf("the installed report was not stored: %v", err)
	}
	if origin != earned.OriginSignedRelease || by != "bench_results_path" {
		t.Fatalf("stored origin %q by %q, want signed_release by bench_results_path",
			origin, by)
	}
}

func TestBenchInstallRefusesABundleSignedForAnotherCommit(t *testing.T) {
	asRelease(t)
	other := strings.Repeat("d", 40)
	withVerifier(t, releaseFakeVerifier{commit: other}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	dest := filepath.Join(t.TempDir(), "bench")
	_, _, err := installBenchReport(src, dest, releaseCommit)
	if !errors.Is(err, earned.ErrSignatureMismatch) {
		t.Fatalf("install of a bundle signed for %s = %v, want ErrSignatureMismatch", other,
			err)
	}
	code, _, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 1 || !strings.Contains(errText, other) {
		t.Fatalf("install = %d, stderr %q; want exit 1 naming commit %s", code, errText,
			other)
	}
	assertNothingInstalled(t, dest)
}

func TestBenchInstallRefusesTheReportOfAnotherBuild(t *testing.T) {
	asRelease(t)
	other := strings.Repeat("e", 40)
	withVerifier(t, releaseFakeVerifier{commit: other}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.4", Commit: other})
	dest := filepath.Join(t.TempDir(), "bench")
	if _, _, err := installBenchReport(src, dest, releaseCommit); !errors.Is(err,
		earned.ErrBuildMismatch) {
		t.Fatalf("install of another build's report = %v, want ErrBuildMismatch", err)
	}
	code, _, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 1 || !strings.Contains(errText, "not "+releaseCommit) {
		t.Fatalf("install = %d, stderr %q", code, errText)
	}
	assertNothingInstalled(t, dest)
}

func TestBenchInstallRefusesATamperedReport(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	raw := readFile(t, src)
	tampered := bytes.Replace(raw, []byte(`"k": 11`), []byte(`"k": 12`), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("the test report has no top-1 count to tamper with")
	}
	if err := os.WriteFile(src, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "bench")
	code, _, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 1 || !strings.Contains(errText, benchsig.ErrInvalidSignature.Error()) {
		t.Fatalf("install of a tampered report = %d, stderr %q", code, errText)
	}
	assertNothingInstalled(t, dest)
}

func TestBenchInstallWithoutABundleSaysSo(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := writeShippedReport(t, t.TempDir(), "live", time.Now().UTC(),
		earned.Build{Version: "1.8.5", Commit: releaseCommit}, "lock_blocking", false)
	dest := filepath.Join(t.TempDir(), "bench")
	code, _, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 1 || !strings.Contains(errText, "no signature bundle") ||
		!strings.Contains(errText, src+benchingest.BundleSuffix) {
		t.Fatalf("install without a bundle = %d, stderr %q", code, errText)
	}
	assertNothingInstalled(t, dest)
}

func TestBenchInstallUnwritablePathNamesThePath(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	// A regular file where a directory is needed: unwritable on every OS.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(blocker, "bench")
	code, _, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 1 || !strings.Contains(errText, dest) {
		t.Fatalf("install into %s = %d, stderr %q; want exit 1 naming the path", dest, code,
			errText)
	}
}

func TestBenchInstallWithoutBenchResultsPathIsUsage(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	code, _, errText := runInstall(t, "--config", installConfig(t, ""), src)
	if code != 2 || !strings.Contains(errText, "bench_results_path") ||
		!strings.Contains(errText, "usage:") {
		t.Fatalf("install without bench_results_path = %d, stderr %q", code, errText)
	}
	// No config file at all (the test's working directory has none).
	code, _, errText = runInstall(t, src)
	if code != 2 || !strings.Contains(errText, "SAGE_CONFIG_PATH") {
		t.Fatalf("install without a config = %d, stderr %q", code, errText)
	}
	for _, args := range [][]string{{}, {"--config", installConfig(t, "x")},
		{"--config", installConfig(t, "x"), src, src}, {"--nope", src}} {
		if code, _, errText := runInstall(t, args...); code != 2 ||
			!strings.Contains(errText, "usage:") {
			t.Errorf("install %q = %d, stderr %q; want usage", args, code, errText)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	if code, _, errText := runInstall(t, "--config", missing, src); code != 2 ||
		!strings.Contains(errText, missing) {
		t.Fatalf("install with a missing config = %d, stderr %q", code, errText)
	}
}

func TestBenchInstallReadsTheConfigFromSageConfigPath(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	dest := filepath.Join(t.TempDir(), "bench")
	cfgPath := installConfig(t, dest)
	getenv := func(k string) string {
		if k == "SAGE_CONFIG_PATH" {
			return cfgPath
		}
		return ""
	}
	var out, errb bytes.Buffer
	if code := runBenchInstall([]string{src}, getenv, &out, &errb); code != 0 {
		t.Fatalf("install = %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dest, filepath.Base(src))); err != nil {
		t.Fatalf("the report was not installed: %v", err)
	}
}

// A dev build names no commit: install needs --commit, then holds the
// report to it.
func TestBenchInstallOnADevBuildNeedsCommit(t *testing.T) {
	oldVersion, oldCommit := version, commit
	version, commit = "dev", "none"
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	dest := filepath.Join(t.TempDir(), "bench")
	cfgPath := installConfig(t, dest)
	code, _, errText := runInstall(t, "--config", cfgPath, src)
	if code != 2 || !strings.Contains(errText, "--commit") {
		t.Fatalf("install on a dev build = %d, stderr %q; want usage naming --commit", code,
			errText)
	}
	assertNothingInstalled(t, dest)
	if code, _, errText := runInstall(t, "--config", cfgPath, "--commit",
		strings.ToUpper(releaseCommit), src); code != 0 {
		t.Fatalf("install --commit = %d, stderr %q", code, errText)
	}
}

// Boundary: a bench_results_path naming a .json file is the file the
// report is installed as; a second install replaces the first (state:
// empty, installed, replaced).
func TestBenchInstallIntoAFilePathReplacesTheInstalledReport(t *testing.T) {
	asRelease(t)
	withVerifier(t, releaseFakeVerifier{commit: releaseCommit}, nil)
	build := earned.Build{Version: "1.8.5", Commit: releaseCommit}
	first := signedRelease(t, build)
	second := writeShippedReport(t, t.TempDir(), "live", time.Now().UTC(), build,
		"wal_retention", true)
	dest := filepath.Join(t.TempDir(), "etc", "live-report.json")
	cfgPath := installConfig(t, dest)
	for _, src := range []string{first, second} {
		if code, _, errText := runInstall(t, "--config", cfgPath, src); code != 0 {
			t.Fatalf("install %s = %d, stderr %q", src, code, errText)
		}
		if !bytes.Equal(readFile(t, dest), readFile(t, src)) ||
			!bytes.Equal(readFile(t, dest+benchingest.BundleSuffix),
				readFile(t, src+benchingest.BundleSuffix)) {
			t.Fatalf("after installing %s the file path holds another report", src)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil || len(entries) != 2 {
		t.Fatalf("%s holds %v (%v), want the report and its bundle, no temp files",
			filepath.Dir(dest), entries, err)
	}
}

func TestBenchInstallWithoutTheTrustedRootFails(t *testing.T) {
	asRelease(t)
	withVerifier(t, nil, errors.New("no trusted root"))
	src := signedRelease(t, earned.Build{Version: "1.8.5", Commit: releaseCommit})
	dest := filepath.Join(t.TempDir(), "bench")
	code, _, errText := runInstall(t, "--config", installConfig(t, dest), src)
	if code != 1 || !strings.Contains(errText, "trusted root") {
		t.Fatalf("install without verification material = %d, stderr %q", code, errText)
	}
	assertNothingInstalled(t, dest)
}

func TestBenchCommandDispatchesInstall(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runBenchCommand([]string{"install"}, &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "pg_sage bench install") {
		t.Fatalf("bench install without args = %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := runBenchCommand(nil, &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "bench install") {
		t.Fatalf("bench usage = %d %q, want it to list install", code, errb.String())
	}
}

// No concurrent-install test: install is a one-shot CLI, and two installs
// of different reports into one path at once are not supported (the
// bundle and the report are each replaced atomically, not as a pair).
