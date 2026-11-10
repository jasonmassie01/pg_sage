package docscontract

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ci-10: no guide asks for an older Go than the `go` directive of
// sidecar/go.mod; the guides once said Go 1.24+ while go.mod required 1.25.
//
// No concurrent-access or state-transition tests: these are static checks
// over the guides and go.mod.

var (
	goDirective    = regexp.MustCompile(`(?m)^go 1\.([0-9]+)(\.[0-9]+)?[ \t]*$`)
	goRequirements = regexp.MustCompile(`\bGo 1\.([0-9]+)\+`)
)

// goDirectiveMinor is the minor version of go.mod's `go` directive.
func goDirectiveMinor(gomod string) (int, error) {
	m := goDirective.FindStringSubmatch(strings.ReplaceAll(gomod, "\r\n", "\n"))
	if m == nil {
		return 0, errors.New("go.mod has no `go 1.N` directive")
	}
	return strconv.Atoi(m[1])
}

// staleGoRequirements returns each "Go 1.N+" in body whose N is below minor.
func staleGoRequirements(body string, minor int) []string {
	var stale []string
	for _, m := range goRequirements.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n < minor {
			stale = append(stale, m[0])
		}
	}
	return stale
}

// goRequirementPages are the Markdown pages that state a Go requirement:
// every page under docs/ plus the root README and CONTRIBUTING.
func goRequirementPages(root string) ([]string, error) {
	pages := []string{filepath.Join(root, "README.md"), filepath.Join(root, "CONTRIBUTING.md")}
	err := filepath.WalkDir(filepath.Join(root, "docs"),
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".md") {
				pages = append(pages, path)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("walk docs: %w", err)
	}
	return pages, nil
}

func TestDocsGoVersionMatchesGoMod(t *testing.T) {
	root := repoRoot(t)
	gomodPath := filepath.Join(root, "sidecar", "go.mod")
	gomod, err := os.ReadFile(gomodPath)
	if err != nil {
		t.Fatalf("read %s: %v", gomodPath, err)
	}
	minor, err := goDirectiveMinor(string(gomod))
	if err != nil {
		t.Fatalf("%s: %v", gomodPath, err)
	}
	pages, err := goRequirementPages(root)
	if err != nil {
		t.Fatal(err)
	}
	mentions := 0
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		mentions += len(goRequirements.FindAllString(string(body), -1))
		if stale := staleGoRequirements(string(body), minor); len(stale) != 0 {
			t.Errorf("%s says %v but sidecar/go.mod requires Go 1.%d", page, stale, minor)
		}
	}
	if mentions < 5 {
		t.Fatalf("found only %d \"Go 1.N+\" requirements; the guides that state one "+
			"(try-it-out, the walkthroughs, installation, README) were not read", mentions)
	}
}

func TestGoDirectiveMinor(t *testing.T) {
	cases := []struct {
		gomod string
		want  int
	}{
		{"module m\n\ngo 1.26.0\n\ntoolchain go1.27.3\n", 26},
		{"module m\r\n\r\ngo 1.25\r\n", 25},
		{"module m\n\ngo 1.9\n", 9},
	}
	for _, tc := range cases {
		if got, err := goDirectiveMinor(tc.gomod); err != nil || got != tc.want {
			t.Errorf("goDirectiveMinor(%q) = %d, %v; want %d", tc.gomod, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "module m\n", "module m\n\ntoolchain go1.27.3\n",
		"module m\n// go 1.26\n"} {
		if got, err := goDirectiveMinor(bad); err == nil {
			t.Errorf("goDirectiveMinor(%q) = %d, want a missing-directive error", bad, got)
		}
	}
}

func TestStaleGoRequirements(t *testing.T) {
	body := "| Go 1.24+ | build |\n- Go 1.26+\nRequires Go 1.27+ and Go 1.25+.\n" +
		"The Dockerfile supplies Go 1.27 (no plus).\n"
	got := staleGoRequirements(body, 26)
	if strings.Join(got, ",") != "Go 1.24+,Go 1.25+" {
		t.Fatalf("stale = %v, want [Go 1.24+ Go 1.25+]", got)
	}
	if got := staleGoRequirements(body, 24); len(got) != 0 {
		t.Fatalf("at minor 24 nothing is stale, got %v", got)
	}
	if got := staleGoRequirements("", 26); len(got) != 0 {
		t.Fatalf("empty page gave %v", got)
	}
}
