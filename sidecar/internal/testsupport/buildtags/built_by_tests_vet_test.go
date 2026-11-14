package buildtags

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// No concurrent access, state-transition or PostgreSQL tests: this runs the
// go tool once per exempt tag over the repository's own packages.

// TestTagsBuiltByTestsAreVetted makes the BuiltByTests exemption true on
// every host: CI builds with cgo, so the e2e suite never passes the tag
// there, and its vet step may not name it. The unit suite therefore vets
// every package that uses an exempt tag, with that tag, so a tagged file
// cannot rot unnoticed (ci-04, ci-18).
func TestTagsBuiltByTestsAreVetted(t *testing.T) {
	root := moduleRoot(t)
	used, err := ScanTree(root)
	if err != nil {
		t.Fatalf("scan build constraints under %s: %v", root, err)
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is needed to vet the exempt tags: %v", err)
	}
	for tag := range BuiltByTests {
		dirs := tagPackageDirs(t, root, used[tag])
		if len(dirs) == 0 {
			t.Fatalf("exempt tag %s constrains no file under %s", tag, root)
		}
		cmd := exec.Command(gotool, append([]string{"vet", "-tags=" + tag}, dirs...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("go vet -tags=%s %s: %v\n%s", tag, strings.Join(dirs, " "), err, out)
		}
	}
}

// tagPackageDirs returns, sorted and unique, the ./-relative package
// directories of files under root.
func tagPackageDirs(t *testing.T, root string, files []string) []string {
	t.Helper()
	var dirs []string
	for _, f := range files {
		rel, err := filepath.Rel(root, filepath.Dir(f))
		if err != nil {
			t.Fatalf("%s is not under %s: %v", f, root, err)
		}
		dirs = append(dirs, "./"+filepath.ToSlash(rel))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}
