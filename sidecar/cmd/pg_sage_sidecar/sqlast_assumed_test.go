package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// executor-10: the e2e suite builds the binary with the test-only tag
// sage_e2e_assume_sqlast on a host without a C compiler. A shipped build
// must never carry it: it would let a cgo-less binary mutate unattended.
// No concurrent access tests: these read a constant and files.

const assumeTag = sqlASTAssumeTag

func TestSQLASTAssumptionIsOffInTheDefaultBuild(t *testing.T) {
	if sqlASTAssumed {
		t.Fatalf("the default build assumes parse-tree validation; only a build " +
			"tagged " + assumeTag + " may")
	}
}

// The tag the e2e suite passes must be the constraint of the file that
// makes the assumption, or the suite silently builds a degraded binary.
func TestSQLASTAssumptionFileIsGatedByTheTag(t *testing.T) {
	data, err := os.ReadFile("sqlast_assumed_e2e.go")
	if err != nil {
		t.Fatalf("read sqlast_assumed_e2e.go: %v", err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	first = strings.TrimSuffix(first, "\r")
	if first != "//go:build "+assumeTag {
		t.Fatalf("sqlast_assumed_e2e.go starts %q, want the %s constraint", first, assumeTag)
	}
}

func TestSQLASTStatusFor(t *testing.T) {
	cases := []struct {
		available, assumed bool
		want               string
	}{
		{true, false, "libpg_query"},
		{false, false, "unavailable (built without cgo)"},
		{true, true, "assumed (e2e test build " + assumeTag + "; never run in production)"},
		{false, true, "assumed (e2e test build " + assumeTag + "; never run in production)"},
	}
	for _, c := range cases {
		if got := sqlASTStatusFor(c.available, c.assumed); got != c.want {
			t.Errorf("available=%v assumed=%v: %q, want %q", c.available, c.assumed,
				got, c.want)
		}
	}
}

// Every build recipe in the repository (image, CI, release and helper
// scripts) is scanned: none may pass the assumption tag.
func TestNoBuildRecipeUsesTheSQLASTAssumptionTag(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	roots := []string{"sidecar/Dockerfile", ".github", ".goreleaser.yml", "scripts",
		"sidecar/scripts", "deploy", "demo", "sidecar/tests", "sidecar/test",
		"docker-compose.yml", "docker-compose.test.yml", "docker-compose.e2e.yml"}
	scanned := 0
	for _, root := range roots {
		path := filepath.Join(repo, filepath.FromSlash(root))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("build recipe %s is missing (moved? update this list): %v", root, err)
		}
		scanned += scanForTag(t, path)
	}
	if scanned < 10 {
		t.Fatalf("scanned only %d files; the recipe roots look wrong", scanned)
	}
}

func scanForTag(t *testing.T, root string) int {
	t.Helper()
	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		if strings.Contains(string(data), assumeTag) {
			t.Errorf("%s passes the test-only tag %s", path, assumeTag)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return files
}
