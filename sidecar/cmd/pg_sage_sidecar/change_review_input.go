package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
)

// What 'pg_sage review submit' sends: migration SQL (--sql-file, or the
// .sql files of --sql-dir, only the ones the diff changes with
// --changed-only) and the unified diff (--diff-file, or git diff of
// --git-diff BASE...HEAD). A change with no SQL and no database-touching
// file in its diff is skipped without calling pg_sage.

const maxReviewFiles = changereview.MaxSQLFiles

// gitRange is BASE...HEAD of refs or SHAs; neither side may look like an
// option.
var gitRange = regexp.MustCompile(
	`^[A-Za-z0-9_][A-Za-z0-9._/~^@{}-]*\.\.\.[A-Za-z0-9_][A-Za-z0-9._/~^@{}-]*$`)

type reviewInput struct {
	files []changereview.SQLFile
	diff  string
}

func gatherReviewInput(ctx context.Context, o reviewOptions, env reviewEnv) (reviewInput,
	error) {
	switch {
	case o.diffFile != "" && o.gitDiff != "":
		return reviewInput{}, usagef("pass --diff-file or --git-diff, not both")
	case o.changedOnly && o.sqlDir == "":
		return reviewInput{}, usagef("--changed-only filters --sql-dir; pass --sql-dir")
	case o.changedOnly && o.diffFile == "" && o.gitDiff == "":
		return reviewInput{}, usagef("--changed-only needs --diff-file or --git-diff")
	}
	var in reviewInput
	var err error
	if in.diff, err = readReviewDiff(ctx, o, env); err != nil {
		return in, err
	}
	for _, p := range o.sqlFiles {
		raw, err := os.ReadFile(p)
		if err != nil {
			return in, usagef("cannot read --sql-file %s: %v", p, errors.Unwrap(err))
		}
		in.files = append(in.files, changereview.SQLFile{Path: filepath.ToSlash(p),
			Content: string(raw)})
	}
	if o.sqlDir != "" {
		files, err := sqlDirFiles(o.sqlDir, o.changedOnly, diffChangedPaths(in.diff))
		if err != nil {
			return in, err
		}
		in.files = append(in.files, files...)
	}
	if len(in.files) > maxReviewFiles {
		return in, usagef("%d SQL files, over the %d a review takes", len(in.files),
			maxReviewFiles)
	}
	if len(in.files) == 0 && strings.TrimSpace(in.diff) == "" && !o.changedOnly {
		return in, usagef("nothing to review: pass --sql-file, --sql-dir, --diff-file " +
			"or --git-diff")
	}
	return in, nil
}

func readReviewDiff(ctx context.Context, o reviewOptions, env reviewEnv) (string, error) {
	switch {
	case o.diffFile != "":
		raw, err := os.ReadFile(o.diffFile)
		if err != nil {
			return "", usagef("cannot read --diff-file %s: %v", o.diffFile, errors.Unwrap(err))
		}
		return string(raw), nil
	case o.gitDiff != "":
		if !gitRange.MatchString(o.gitDiff) {
			return "", usagef("--git-diff must be BASE...HEAD, got %q", o.gitDiff)
		}
		out, err := env.git(ctx, "diff", "--no-color", "--no-ext-diff", o.gitDiff)
		if err != nil {
			return "", usagef("git diff %s failed: %v", o.gitDiff, err)
		}
		return string(out), nil
	}
	return "", nil
}

// sqlDirFiles are the .sql files under dir in path order; with changedOnly
// only those among changed (repository-relative paths of the diff).
func sqlDirFiles(dir string, changedOnly bool, changed []string) ([]changereview.SQLFile,
	error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".sql") {
			paths = append(paths, filepath.ToSlash(filepath.Clean(p)))
		}
		return nil
	})
	if err != nil {
		return nil, usagef("cannot read --sql-dir %s: %v", dir, err)
	}
	sort.Strings(paths)
	var out []changereview.SQLFile
	for _, p := range paths {
		if changedOnly && !pathChanged(p, changed) {
			continue
		}
		raw, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			return nil, usagef("cannot read %s: %v", p, errors.Unwrap(err))
		}
		out = append(out, changereview.SQLFile{Path: p, Content: string(raw)})
	}
	return out, nil
}

// pathChanged matches a file against the diff's paths (the file may sit
// under a checkout prefix the diff does not have).
func pathChanged(file string, changed []string) bool {
	for _, c := range changed {
		if file == c || strings.HasSuffix(file, "/"+c) {
			return true
		}
	}
	return false
}

// diffChangedPaths are the files a unified diff changes, from its
// "--- a/x" / "+++ b/y" header pairs (both sides; /dev/null skipped).
func diffChangedPaths(diff string) []string {
	lines := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	seen := map[string]bool{}
	var out []string
	add := func(raw, prefix string) {
		p := strings.TrimSpace(strings.SplitN(raw, "\t", 2)[0])
		p = strings.TrimPrefix(p, prefix)
		if p == "" || p == "/dev/null" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for i := 0; i+1 < len(lines); i++ {
		if strings.HasPrefix(lines[i], "--- ") && strings.HasPrefix(lines[i+1], "+++ ") {
			add(lines[i][4:], "a/")
			add(lines[i+1][4:], "b/")
			i++
		}
	}
	return out
}

// touchesDatabase: SQL was given, or the diff changes a data-access file
// by diffscan's classifier, the same one that picks the hunks offered to
// the model: migrations, *.sql, models, repositories, DAOs, or code whose
// added lines call an ORM or embed SQL.
func (in reviewInput) touchesDatabase() bool {
	return len(in.files) > 0 || diffscan.TouchesDatabase(in.diff)
}
