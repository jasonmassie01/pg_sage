package secretscan

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// repoRoot is the repository root, three levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sidecar", "go.mod")); err != nil {
		t.Fatalf("repository root %s not found: %v", root, err)
	}
	return root
}

// G0-06: none of the AU-10 secrets is in any tracked file. The check knows
// only their SHA-256 hashes and lengths; a failure names file and line.
func TestTrackedFilesContainNoAU10Secrets(t *testing.T) {
	root := repoRoot(t)
	files, err := TrackedFiles(root)
	if err != nil {
		t.Fatalf("listing tracked files: %v", err)
	}
	if len(files) < 100 {
		t.Fatalf("only %d tracked files listed under %s; the listing is broken", len(files),
			root)
	}
	hits, err := ScanFiles(root, files, AU10)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, hit := range hits {
		t.Errorf("AU-10 secret found at %s; replace it with a placeholder or an "+
			"environment lookup", hit)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTrackedFiles_WalksWhenNotAGitCheckout(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.txt":                    "a",
		"docs/b.md":                "b",
		".git/config":              "skip",
		"web/node_modules/x/y.js":  "skip",
		"sidecar/.gocache/z":       "skip",
		"deep/nested/dir/file.ps1": "c",
	})
	files, err := walkFiles(root)
	if err != nil {
		t.Fatalf("walkFiles: %v", err)
	}
	sort.Strings(files)
	want := []string{"a.txt", "deep/nested/dir/file.ps1", "docs/b.md"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", files, want)
	}
}

func TestScanFiles_ReportsRelativePathsAndLines(t *testing.T) {
	root := writeTree(t, map[string]string{
		"clean.md":       "nothing here\n",
		"dir/leaky.ps1":  "param(\n  [string]$P = \"" + dummy + "\"\n)\n",
		"dir/leaky2.txt": dummy,
	})
	hits, err := ScanFiles(root, []string{"clean.md", "dir/leaky.ps1", "dir/leaky2.txt"},
		[]Target{targetFor(dummy)})
	if err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}
	got := make([]string, 0, len(hits))
	for _, hit := range hits {
		got = append(got, hit.String())
	}
	want := "dir/leaky.ps1:2 (target 0),dir/leaky2.txt:1 (target 0)"
	if strings.Join(got, ",") != want {
		t.Fatalf("hits = %v, want %s", got, want)
	}
}

func TestScanFiles_UnreadablePathIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "a", "dir/b.txt": "b"})
	_, err := ScanFiles(root, []string{"a.txt", "dir"}, []Target{targetFor(dummy)})
	if err == nil || !strings.Contains(err.Error(), "dir") {
		t.Fatalf("err = %v, want one naming dir", err)
	}
}

func TestScanFiles_InvalidTargetsAreAnError(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "a"})
	if _, err := ScanFiles(root, []string{"a.txt"}, nil); err == nil {
		t.Fatal("ScanFiles accepted no targets")
	}
}

// A file deleted in the working tree but still in the index is skipped, not
// an error: `git ls-files` lists it until the deletion is staged.
func TestScanFiles_SkipsTrackedFileDeletedFromWorkTree(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": dummy})
	hits, err := ScanFiles(root, []string{"deleted.txt", "a.txt"}, []Target{targetFor(dummy)})
	if err != nil || len(hits) != 1 || hits[0].Path != "a.txt" {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
}
