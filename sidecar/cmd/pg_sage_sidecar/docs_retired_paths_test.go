package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// cloudsqltests: the operator docs never name a repository path that was
// deleted. ci-04 retired the v0.8 cloudsqltests/ scripts, and the C
// extension's src/ and sql/ directories are gone too; a doc that cites them
// describes code nobody can read. Historical records (reviews/, research/,
// CHANGELOG.md) live outside docs/ and are not scanned.

// No invalid-input, concurrency, state-transition or integration tests: this
// is a static scan over markdown files with no shared state or database.

// retiredRepoPaths are top-level repository directories that no longer exist.
var retiredRepoPaths = []string{"cloudsqltests/", "src/", "sql/"}

// retiredPathToken matches a retired path only as the start of a path token
// (line start, space, backtick or parenthesis before it), so a nested path
// such as sidecar/web/src/App.jsx is never a hit.
var retiredPathToken = regexp.MustCompile("(?m)(?:^|[ `(])(cloudsqltests/|src/|sql/)")

// retiredPathHits scans every *.md file under fsys and returns one
// "file:line: names retired path X" entry per match.
func retiredPathHits(fsys fs.FS) ([]string, error) {
	var hits []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(name, ".md") {
			return nil
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range retiredPathToken.FindAllStringSubmatch(line, -1) {
				hits = append(hits, name+":"+strconv.Itoa(i+1)+
					": names retired path "+m[1])
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

func TestDocsNameNoRetiredRepoPaths(t *testing.T) {
	for _, p := range retiredRepoPaths {
		_, err := os.Stat(filepath.Join(docsRoot, p))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s exists again (stat err %v); drop it from retiredRepoPaths", p, err)
		}
	}
	hits, err := retiredPathHits(os.DirFS(filepath.Join(docsRoot, "docs")))
	if err != nil {
		t.Fatalf("scan docs/: %v", err)
	}
	for _, h := range hits {
		t.Errorf("docs/%s", h)
	}
}

func TestRetiredPathHits_TokenBoundaries(t *testing.T) {
	fsys := fstest.MapFS{
		"a.md": {Data: []byte("intro\nsee `cloudsqltests/setup.sh` here\n" +
			"src/pg_sage.c at line start\nthe (sql/install.sql) file\n" +
			"nested sidecar/web/src/App.jsx and mysql/ and pgsql/x\n")},
		"sub/b.md":  {Data: []byte("run sql/x.sql and src/y.c\n")},
		"notes.txt": {Data: []byte("cloudsqltests/ in a non-markdown file\n")},
	}
	hits, err := retiredPathHits(fsys)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{
		"a.md:2: names retired path cloudsqltests/",
		"a.md:3: names retired path src/",
		"a.md:4: names retired path sql/",
		"sub/b.md:1: names retired path sql/",
		"sub/b.md:1: names retired path src/",
	}
	if strings.Join(hits, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hits:\n%s\nwant:\n%s", strings.Join(hits, "\n"), strings.Join(want, "\n"))
	}
}

// Boundary: historical records outside docs/ keep their mentions and are not
// scanned, because the scan is rooted at docs/.
func TestRetiredPathHits_HistoricalRecordsNotScanned(t *testing.T) {
	repo := fstest.MapFS{
		"CHANGELOG.md":         {Data: []byte("- removed `cloudsqltests/`\n")},
		"reviews/r.md":         {Data: []byte("the (cloudsqltests/) scripts\n")},
		"research/spec.md":     {Data: []byte("src/pg_sage.c\n")},
		"docs/clean.md":        {Data: []byte("nothing retired here\n")},
		"docs/generated/g.md":  {Data: []byte("sidecar/web/src/x.jsx\n")},
		"docs/generated/n.txt": {Data: []byte("sql/old.sql\n")},
	}
	docs, err := fs.Sub(repo, "docs")
	if err != nil {
		t.Fatalf("sub docs: %v", err)
	}
	hits, err := retiredPathHits(docs)
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits %v, err %v; want none", hits, err)
	}
	all, err := retiredPathHits(repo)
	if err != nil || len(all) != 3 {
		t.Fatalf("whole-repo scan: hits %v, err %v; want the 3 historical mentions", all, err)
	}
}

func TestRetiredPathHits_NoMarkdown(t *testing.T) {
	for name, fsys := range map[string]fs.FS{
		"empty dir":   fstest.MapFS{},
		"no markdown": fstest.MapFS{"x.txt": {Data: []byte("cloudsqltests/\n")}},
		"empty file":  fstest.MapFS{"e.md": {Data: nil}},
	} {
		hits, err := retiredPathHits(fsys)
		if err != nil || hits != nil {
			t.Errorf("%s: hits %v, err %v; want nil, nil", name, hits, err)
		}
	}
}

// failingFS fails to open one named file, as an unreadable doc would.
type failingFS struct {
	fstest.MapFS
	bad string
}

var errUnreadable = errors.New("permission denied")

func (f failingFS) Open(name string) (fs.File, error) {
	if name == f.bad {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errUnreadable}
	}
	return f.MapFS.Open(name)
}

// ReadFile shadows MapFS.ReadFile, which fs.ReadFile would otherwise call
// without going through Open.
func (f failingFS) ReadFile(name string) ([]byte, error) {
	if name == f.bad {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errUnreadable}
	}
	return f.MapFS.ReadFile(name)
}

func TestRetiredPathHits_UnreadableFileFailsWithPath(t *testing.T) {
	fsys := failingFS{
		MapFS: fstest.MapFS{
			"ok.md":         {Data: []byte("fine\n")},
			"guides/bad.md": {Data: []byte("unreachable\n")},
		},
		bad: "guides/bad.md",
	}
	hits, err := retiredPathHits(fsys)
	if !errors.Is(err, errUnreadable) {
		t.Fatalf("err %v; want the open error", err)
	}
	if !strings.Contains(err.Error(), "guides/bad.md") {
		t.Fatalf("err %q does not name the unreadable file", err)
	}
	if hits != nil {
		t.Fatalf("hits %v; want nil on error", hits)
	}
}
