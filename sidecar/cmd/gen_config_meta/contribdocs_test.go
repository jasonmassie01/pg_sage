package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// ci-10: -contrib-docs regenerates the "Test environment" table of
// CONTRIBUTING.md from the contributor entries of the registry, and -check
// reports its drift. With neither -env-docs nor -contrib-docs, -check reads
// both checked-in files.
//
// No concurrent-access tests: the generator is a one-shot build tool.

func writeContribDoc(t *testing.T, table string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CONTRIBUTING.md")
	body := "# Contributing\r\n\r\n" + config.ContributorEnvDocsStart + "\r\n" + table +
		config.ContributorEnvDocsEnd + "\r\n\r\nafter\r\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunWritesTheContributorTableKeepingCRLF(t *testing.T) {
	path := writeContribDoc(t, "| stale |\r\n")
	if err := run([]string{"-env-only", "-contrib-docs", path}); err != nil {
		t.Fatalf("run -contrib-docs: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Contributing\n\n" + config.ContributorEnvDocsStart + "\n" +
		config.ContributorEnvVarsMarkdown() + config.ContributorEnvDocsEnd + "\n\nafter\n"
	if string(body) != strings.ReplaceAll(want, "\n", "\r\n") {
		t.Fatalf("CONTRIBUTING after -contrib-docs = %q", body)
	}
	if err := run([]string{"-check", "-contrib-docs", path}); err != nil {
		t.Fatalf("-check after regeneration: %v", err)
	}
}

func TestCheckReportsContributorDriftAndWritesNothing(t *testing.T) {
	path := writeContribDoc(t, "| `SAGE_TEST_DATABASE_URL` | (none) | stale |\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-check", "-contrib-docs", path})
	if err == nil || !strings.Contains(err.Error(), "drift") ||
		!strings.Contains(err.Error(), "-contrib-docs") {
		t.Fatalf("-check on a stale table = %v, want a drift error naming the fix", err)
	}
	if after, readErr := os.ReadFile(path); readErr != nil || string(after) != string(before) {
		t.Fatalf("-check rewrote the file (err %v)", readErr)
	}
}

func TestContributorDocsErrorsAreActionable(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.md")
	for _, args := range [][]string{
		{"-check", "-contrib-docs", missing},
		{"-env-only", "-contrib-docs", missing},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "missing.md") {
			t.Errorf("run(%v) = %v, want an error naming the file", args, err)
		}
	}
	// The runtime table's markers do not satisfy the contributor table.
	runtimeOnly := filepath.Join(dir, "configuration.md")
	body := config.EnvDocsStart + "\n" + config.EnvVarsMarkdown() + config.EnvDocsEnd + "\n"
	if err := os.WriteFile(runtimeOnly, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-check", "-contrib-docs", runtimeOnly}); err == nil ||
		!strings.Contains(err.Error(), "marker") {
		t.Fatalf("-check on a file without contributor markers = %v, want a marker error", err)
	}
}

// CHECK-09: the checked-in CONTRIBUTING.md carries no drift.
func TestCheckedInContributingHasNoContributorDrift(t *testing.T) {
	path := filepath.Join("..", "..", "..", "CONTRIBUTING.md")
	if err := run([]string{"-check", "-contrib-docs", path}); err != nil {
		t.Fatalf("CONTRIBUTING.md: %v", err)
	}
}

// With neither docs flag, -check reads both default files: a drifted
// contributor table fails even when the runtime table is current.
func TestCheckWithoutFlagsAlsoChecksContributing(t *testing.T) {
	root := t.TempDir()
	sidecar := filepath.Join(root, "sidecar")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeDoc := config.EnvDocsStart + "\n" + config.EnvVarsMarkdown() + config.EnvDocsEnd + "\n"
	err := os.WriteFile(filepath.Join(root, "docs", "configuration.md"), []byte(runtimeDoc), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	contrib := func(table string) {
		t.Helper()
		body := config.ContributorEnvDocsStart + "\n" + table + config.ContributorEnvDocsEnd + "\n"
		path := filepath.Join(root, "CONTRIBUTING.md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(sidecar)
	contrib("| stale |\n")
	err = run([]string{"-check"})
	if err == nil || !strings.Contains(err.Error(), "CONTRIBUTING.md") ||
		!strings.Contains(err.Error(), "contributor") {
		t.Fatalf("-check with a stale CONTRIBUTING.md = %v, want a contributor drift error", err)
	}
	contrib(config.ContributorEnvVarsMarkdown())
	if err := run([]string{"-check"}); err != nil {
		t.Fatalf("-check with both tables current: %v", err)
	}
}
