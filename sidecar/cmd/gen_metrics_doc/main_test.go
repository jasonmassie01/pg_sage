package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-09: the generated metrics reference. No concurrent-access
// tests: the generator is a one-shot build tool.

const repoRoot = "../../.."

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// Freshness (CHECK-24): the checked-in reference is what go generate
// writes, so the registry and docs/metrics.md cannot drift.
func TestCheckedInMetricsDocIsCurrent(t *testing.T) {
	doc := filepath.Join(repoRoot, "docs", "metrics.md")
	if err := run([]string{"-check", "-out", doc}); err != nil {
		t.Fatalf("docs/metrics.md: %v", err)
	}
}

func TestRenderListsEveryFamilyOnceUnderItsArea(t *testing.T) {
	doc := render(metricsreg.Families(), metricsreg.Areas(), metricsreg.RetiredFamilies(),
		runbooks)
	for _, f := range metricsreg.Families() {
		row := "| `" + f.Name + "` | " + string(f.Type) + " |"
		if n := strings.Count(doc, row); n != 1 {
			t.Errorf("%s has %d rows, want 1", f.Name, n)
		}
	}
	for _, a := range metricsreg.Areas() {
		if !strings.Contains(doc, "\n## "+a.Title+"\n") {
			t.Errorf("area %q has no section", a.Title)
		}
	}
	for _, r := range runbooks {
		if !strings.Contains(doc, "\n### "+r.Alert+"\n") {
			t.Errorf("alert %s has no runbook section", r.Alert)
		}
	}
	for _, r := range metricsreg.RetiredFamilies() {
		if !strings.Contains(doc, "| `"+r.Name+"` |") {
			t.Errorf("retired %s is not in the rename table", r.Name)
		}
	}
	if !strings.Contains(doc, "| `pg_sage_optimizer_recommendations` | gauge | `category` | "+
		"- | standalone only |") {
		t.Error("a standalone-only family is not marked as such")
	}
}

// Nil/empty: an empty registry still renders the page skeleton.
func TestRenderEmptyRegistry(t *testing.T) {
	doc := render(nil, nil, nil, nil)
	if !strings.HasPrefix(doc, "# Metrics reference\n") ||
		!strings.Contains(doc, renamesStart) || strings.Contains(doc, "| `pg_sage_") {
		t.Fatalf("empty render:\n%s", doc)
	}
}

func TestCellEscapesPipes(t *testing.T) {
	if got := cell("a|b"); got != `a\|b` {
		t.Fatalf("cell = %q, want a\\|b", got)
	}
	if got := codeList(nil); got != "-" {
		t.Fatalf("codeList(nil) = %q, want -", got)
	}
}

func TestRunWritesAndCheckReportsDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.md")
	if err := run([]string{"-out", path}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := run([]string{"-check", "-out", path}); err != nil {
		t.Fatalf("check after write: %v", err)
	}
	crlf := strings.ReplaceAll(readFile(t, path), "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-check", "-out", path}); err != nil {
		t.Fatalf("check of a CRLF checkout: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-check", "-out", path})
	if !errors.Is(err, errDrift) || !strings.Contains(err.Error(), "go generate") {
		t.Fatalf("check of a stale file = %v, want errDrift naming go generate", err)
	}
	if readFile(t, path) != "stale\n" {
		t.Fatal("-check wrote the file")
	}
}

// Error propagation: a missing file and a bad flag are reported.
func TestRunErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "metrics.md")
	if err := run([]string{"-check", "-out", missing}); err == nil ||
		!strings.Contains(err.Error(), "read") {
		t.Fatalf("check of a missing file = %v, want a read error", err)
	}
	if err := run([]string{"-out", missing}); err == nil ||
		!strings.Contains(err.Error(), "write") {
		t.Fatalf("write into a missing directory = %v, want a write error", err)
	}
	if err := run([]string{"-bogus"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

// docs/deployment.md carries the same rename table as the reference.
func TestDeploymentDocCarriesTheRenameTable(t *testing.T) {
	want := renamesTable(metricsreg.RetiredFamilies())
	if doc := readRepo(t, "docs/deployment.md"); !strings.Contains(doc, want) {
		t.Fatalf("docs/deployment.md lacks the generated rename table:\n%s", want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
