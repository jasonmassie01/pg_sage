package schema

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentgov-04: the declarative principals-file package bypassed the kill
// and retire paths and had no caller, so it was removed. Declarative
// principals are a G2 roadmap item that must be built on agentguard's
// Switch and retire path; this guard keeps the old package from coming
// back unnoticed. The name is split so a repository grep for it stays
// empty.
var removedPackages = []string{"principal" + "file"}

// No concurrent access test: the walk reads the checkout only.

func TestRemovedPackages_NotInModule(t *testing.T) {
	root := filepath.Join("..", "..") // the sidecar module root
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found from internal/schema: %v", err)
	}
	var files int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		checkRemovedPackageFile(t, path)
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipDir) {
		t.Fatalf("walk the module: %v", err)
	}
	if files < 100 {
		t.Fatalf("walk saw only %d Go files; the guard looked in the wrong place", files)
	}
}

// checkRemovedPackageFile fails when a Go file lives in, or imports, a
// removed package.
func checkRemovedPackageFile(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Base(filepath.Dir(path))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, name := range removedPackages {
		if dir == name {
			t.Errorf("%s: package %s was removed (agentgov-04)", path, name)
		}
		if strings.Contains(string(src), "/internal/"+name+`"`) {
			t.Errorf("%s imports the removed package %s (agentgov-04)", path, name)
		}
	}
}
