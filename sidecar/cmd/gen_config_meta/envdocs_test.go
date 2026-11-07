package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-08: the generator writes the environment-variable table into
// docs/configuration.md between its markers, and -check reports drift
// without writing anything.
//
// No concurrent-access tests: the generator is a one-shot build tool.

func writeEnvDoc(t *testing.T, table string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configuration.md")
	body := "# Configuration\n\n" + config.EnvDocsStart + "\n" + table +
		config.EnvDocsEnd + "\n\nafter\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunWritesTheEnvTableIntoTheDocs(t *testing.T) {
	path := writeEnvDoc(t, "| stale | table |\n")
	if err := run([]string{"-env-docs", path, "-env-only"}); err != nil {
		t.Fatalf("run -env-docs: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Configuration\n\n" + config.EnvDocsStart + "\n" + config.EnvVarsMarkdown() +
		config.EnvDocsEnd + "\n\nafter\n"
	if string(body) != want {
		t.Fatalf("docs after -env-docs = %q, want %q", body, want)
	}
	if err := run([]string{"-check", "-env-docs", path}); err != nil {
		t.Fatalf("-check after regeneration: %v", err)
	}
}

func TestCheckReportsDriftAndWritesNothing(t *testing.T) {
	path := writeEnvDoc(t, "| `SAGE_PG_PORT` | (none) | stale |\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-check", "-env-docs", path})
	if err == nil || !strings.Contains(err.Error(), "drift") ||
		!strings.Contains(err.Error(), "-env-docs") {
		t.Fatalf("-check on a stale table = %v, want a drift error naming the fix", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("-check rewrote the file (err %v)", readErr)
	}
}

func TestEnvDocsErrorsAreActionable(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.md")
	if err := run([]string{"-check", "-env-docs", missing}); err == nil ||
		!strings.Contains(err.Error(), "missing.md") {
		t.Fatalf("-check on a missing file = %v, want an error naming it", err)
	}
	noMarkers := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(noMarkers, []byte("# no markers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-check", "-env-docs", noMarkers},
		{"-env-only", "-env-docs", noMarkers},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "marker") {
			t.Errorf("run(%v) = %v, want a marker error", args, err)
		}
	}
	if err := run([]string{"-env-only"}); err == nil ||
		!strings.Contains(err.Error(), "-env-docs") {
		t.Fatalf("-env-only without -env-docs = %v, want a usage error", err)
	}
}

// CHECK-25: the checked-in docs carry no drift (the default -check path is
// relative to sidecar/, this test runs from sidecar/cmd/gen_config_meta).
func TestCheckedInConfigurationDocsHaveNoEnvDrift(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "configuration.md")
	if err := run([]string{"-check", "-env-docs", path}); err != nil {
		t.Fatalf("docs/configuration.md: %v", err)
	}
}

func TestCheckDefaultsToTheRepositoryDocs(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	if err := run([]string{"-check"}); err != nil {
		t.Fatalf("-check from sidecar/: %v", err)
	}
}
