package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A newcomer who installs the Go release a doc names must be able to build:
// every "Go 1.NN+" prerequisite in docs/ and README.md must name at least
// the minor version sidecar/go.mod requires.

// No concurrent access tests: these helpers are pure functions over strings
// and a read-only directory walk.

const (
	goModPath   = "../../go.mod"
	docsRootDir = "../../../docs"
	readmePath  = "../../../README.md"
)

var (
	goDirective  = regexp.MustCompile(`(?m)^go\s+1\.(\d+)(?:\.\d+)?\s*$`)
	goPrereq     = regexp.MustCompile(`\bGo 1\.(\d+)(?:\.\d+)?\+`)
	datedDocName = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	// Reverse specs and reports describe the tree as it was when written.
	skippedDocDirs = map[string]bool{"reverse_spec": true, "reports": true}
)

// goModMinor is the minor version of go.mod's go directive.
func goModMinor(goMod string) (int, error) {
	m := goDirective.FindStringSubmatch(goMod)
	if m == nil {
		return 0, fmt.Errorf("go.mod has no \"go 1.NN\" directive")
	}
	return strconv.Atoi(m[1])
}

// staleGoPrereqs lists the "Go 1.NN+" mentions in doc, with their line
// numbers, that name a minor version below minMinor.
func staleGoPrereqs(doc string, minMinor int) []string {
	var out []string
	for i, line := range strings.Split(doc, "\n") {
		for _, m := range goPrereq.FindAllStringSubmatch(line, -1) {
			if v, err := strconv.Atoi(m[1]); err == nil && v < minMinor {
				out = append(out, fmt.Sprintf("line %d: %q", i+1, m[0]))
			}
		}
	}
	return out
}

// goPrereqDocs lists the Markdown files under root that state current
// prerequisites: everything except reverse specs, reports and dated files.
func goPrereqDocs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skippedDocDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") && !datedDocName.MatchString(d.Name()) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list docs under %s: %w", root, err)
	}
	return out, nil
}

func TestDocsGoVersionMatchesGoMod(t *testing.T) {
	raw, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read %s: %v", goModPath, err)
	}
	minor, err := goModMinor(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", goModPath, err)
	}
	docs, err := goPrereqDocs(docsRootDir)
	if err != nil {
		t.Fatal(err)
	}
	mentions := 0
	for _, path := range append(docs, readmePath) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		mentions += len(goPrereq.FindAllString(string(body), -1))
		for _, s := range staleGoPrereqs(string(body), minor) {
			t.Errorf("%s %s: go.mod requires Go 1.%d", path, s, minor)
		}
	}
	// The walkthroughs, try-it-out, installation and README all state it.
	if mentions < 6 {
		t.Fatalf("found %d \"Go 1.NN+\" prerequisites, want at least 6: the doc "+
			"walk or the pattern is broken", mentions)
	}
}

func TestDocsGoVersionGoModMinor(t *testing.T) {
	cases := []struct {
		name, goMod string
		want        int
		wantErr     bool
	}{
		{"patch", "module x\n\ngo 1.26.0\n\ntoolchain go1.27.3\n", 26, false},
		{"no patch", "module x\ngo 1.25\n", 25, false},
		{"crlf", "module x\r\ngo 1.24.1\r\n", 24, false},
		{"toolchain only", "module x\ntoolchain go1.27.3\n", 0, true},
		{"empty", "", 0, true},
		{"commented", "module x\n// go 1.26\n", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := goModMinor(c.goMod)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), `"go 1.NN" directive`) {
					t.Fatalf("goModMinor(%q) = %d, %v; want a go directive error",
						c.goMod, got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("goModMinor(%q) = %d, %v; want %d", c.goMod, got, err, c.want)
			}
		})
	}
}

func TestDocsGoVersionStaleMentions(t *testing.T) {
	cases := []struct {
		name, doc string
		want      []string
	}{
		{"older minor", "## Prerequisites\n\n- Go 1.24+\n", []string{`line 3: "Go 1.24+"`}},
		{"table row", "| Go 1.25+ | Build |", []string{`line 1: "Go 1.25+"`}},
		{"older patch form", "use Go 1.25.3+ here", []string{`line 1: "Go 1.25.3+"`}},
		{"two on a line", "Go 1.20+ or Go 1.21+", []string{`line 1: "Go 1.20+"`,
			`line 1: "Go 1.21+"`}},
		{"equal minor", "- Go 1.26+\n", nil},
		{"newer minor", "- Go 1.27+\n", nil},
		{"no go mention", "# Fleet\n\nDocker and Node.js 22+.\n", nil},
		{"empty", "", nil},
		{"exact version is not a prerequisite", "built on Go 1.24, then", nil},
		{"lower-case word", "we go 1.20+ miles", nil},
		{"two-digit boundary", "Go 1.3+", []string{`line 1: "Go 1.3+"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := staleGoPrereqs(c.doc, 26)
			if strings.Join(got, ";") != strings.Join(c.want, ";") {
				t.Fatalf("staleGoPrereqs(%q, 26) = %q, want %q", c.doc, got, c.want)
			}
		})
	}
}

func TestDocsGoVersionDocSelection(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"walkthrough.md":                  "Go 1.20+",
		"sub/guide.md":                    "Go 1.20+",
		"notes.txt":                       "Go 1.20+",
		"reverse_spec/old.md":             "Go 1.20+",
		"reports/qa.md":                   "Go 1.20+",
		"plans/2026-05-09-fresh-local.md": "Go 1.20+",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := goPrereqDocs(root)
	if err != nil {
		t.Fatalf("goPrereqDocs: %v", err)
	}
	var rel []string
	for _, p := range got {
		r, _ := filepath.Rel(root, p)
		rel = append(rel, filepath.ToSlash(r))
	}
	if strings.Join(rel, ",") != "sub/guide.md,walkthrough.md" {
		t.Fatalf("goPrereqDocs selected %v, want [sub/guide.md walkthrough.md]", rel)
	}
}

func TestDocsGoVersionMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-docs")
	got, err := goPrereqDocs(missing)
	if err == nil || !strings.Contains(err.Error(), "no-such-docs") {
		t.Fatalf("goPrereqDocs(missing) = %v, %v; want an error naming the path", got, err)
	}
}
