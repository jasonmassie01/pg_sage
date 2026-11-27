package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/config"
)

// pg_sage bench install (benches-09): verifies a signed release report
// (the release's pgincidentbench-live.json and its .sigstore.json) for
// the running commit, then copies both into sre.autonomy.bench_results_path,
// where the hourly ingester stores the report as a signed release report.
// The copy is atomic per file (temp file plus rename), bundle first, so the
// ingester never reads a report without its bundle.

const benchInstallCommand = "pg_sage bench install [--config FILE] [--commit SHA] " +
	"<pgincidentbench.json>"

// installFileMode lets the sidecar's user read the installed (public)
// report and bundle.
const installFileMode = 0o644

func runBenchInstall(args []string, getenv func(string) string, stdout,
	stderr io.Writer) int {
	usage := func(msg string) int {
		if msg != "" {
			_, _ = fmt.Fprintf(stderr, "bench install: %s\n", msg)
		}
		_, _ = fmt.Fprintln(stderr, "usage: "+benchInstallCommand)
		return 2
	}
	fs := flag.NewFlagSet("bench install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "the sidecar's config.yaml (default: "+
		"SAGE_CONFIG_PATH, then ./config.yaml)")
	want := fs.String("commit", "", "the commit the report must be built from "+
		"(default: this binary's)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return usage("")
	}
	dest, err := benchInstallDestination(*cfgPath, getenv)
	if err != nil {
		return usage(err.Error())
	}
	commitWant := strings.TrimSpace(*want)
	if commitWant == "" {
		commitWant = runningBuild().Commit
	}
	if commitWant == "" {
		return usage("this pg_sage build names no commit; pass --commit SHA, the commit " +
			"of the sidecar that ingests the report")
	}
	report := fs.Arg(0)
	target, sig, err := installBenchReport(report, dest, commitWant)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bench install %s: %v\n", report, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "installed %s as %s: signed by %s, commit %s; the sidecar "+
		"ingests it at startup and hourly\n", report, target, sig.Identity, sig.Commit)
	return 0
}

// benchInstallDestination is bench_results_path from the config file:
// --config, else SAGE_CONFIG_PATH, else ./config.yaml.
func benchInstallDestination(path string, getenv func(string) string) (string, error) {
	if path == "" {
		path = getenv("SAGE_CONFIG_PATH")
	}
	if path == "" {
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	if path == "" {
		return "", errors.New("no config file: pass --config FILE or set SAGE_CONFIG_PATH")
	}
	dest, err := config.BenchResultsPathFromFile(path)
	if err != nil {
		return "", fmt.Errorf("read sre.autonomy.bench_results_path from %s: %w", path, err)
	}
	if dest == "" {
		return "", fmt.Errorf("sre.autonomy.bench_results_path is not set in %s; set it to "+
			"the file or directory the sidecar ingests bench reports from", path)
	}
	return dest, nil
}

// installBenchReport verifies report for commit want and copies exactly
// the verified bytes into dest: a directory (created when missing) keeps
// the report's name, a .json path is the installed report's name. It
// returns the installed report's path.
func installBenchReport(report, dest, want string) (string, benchsig.Signature, error) {
	sig, v, err := readVerifiedReport(report, want)
	if err != nil {
		return "", benchsig.Signature{}, err
	}
	dir, name := dest, filepath.Base(report)
	if strings.EqualFold(filepath.Ext(dest), ".json") {
		dir, name = filepath.Dir(dest), filepath.Base(dest)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", sig, fmt.Errorf("bench_results_path %s: %w", dest, err)
	}
	target := filepath.Join(dir, name)
	if err := writeFileAtomic(target+benchingest.BundleSuffix, v.bundle); err != nil {
		return "", sig, fmt.Errorf("bench_results_path %s: %w", dest, err)
	}
	if err := writeFileAtomic(target, v.report); err != nil {
		return "", sig, fmt.Errorf("bench_results_path %s: %w", dest, err)
	}
	return target, sig, nil
}

// writeFileAtomic replaces path with data through a temp file in its
// directory (named *.tmp, which the ingester's walk passes over).
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create a temp file for %s: %w", path, err)
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Chmod(installFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	done = true
	return nil
}
