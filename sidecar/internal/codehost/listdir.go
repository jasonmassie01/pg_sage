package codehost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

// GitLab's tree pages: glTreePage entries a page, glTreeMaxPages pages at
// most (MaxDirEntries entries).
const (
	glTreePage     = 100
	glTreeMaxPages = MaxDirEntries / glTreePage
)

type dirEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ListDir implements Host with GET /repos/{repo}/contents/{dir}, which
// answers a directory with an array of its entries (at most 1,000).
func (g *GitHub) ListDir(ctx context.Context, repo, dir string) ([]string, error) {
	base, err := ghRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := ValidatePath(dir); err != nil {
		return nil, err
	}
	raw, err := g.r.do(ctx, call{method: http.MethodGet,
		path: base + "/contents/" + escapePath(dir), repo: repo})
	if err != nil {
		return nil, err
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' {
		return nil, fmt.Errorf("%w: %s in %s is not a directory", ErrNotFound, dir, repo)
	}
	var entries []dirEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%w: codehost %s: contents of %s in %s: unexpected answer: %v",
			ErrUnavailable, g.Name(), dir, repo, err)
	}
	return fileNames(entries, "file", dir, repo)
}

// ListDir implements Host with the repository tree of dir on the default
// branch, page by page. GitLab answers an unknown path with an empty list.
func (g *GitLab) ListDir(ctx context.Context, repo, dir string) ([]string, error) {
	base, err := glProject(repo)
	if err != nil {
		return nil, err
	}
	if err := ValidatePath(dir); err != nil {
		return nil, err
	}
	var all []dirEntry
	for page := 1; ; page++ {
		if page > glTreeMaxPages {
			return nil, fmt.Errorf("%w: %s in %s has more than %d entries", ErrTooLarge,
				dir, repo, MaxDirEntries)
		}
		q := url.Values{"path": {dir}, "ref": {"HEAD"}, "page": {strconv.Itoa(page)},
			"per_page": {strconv.Itoa(glTreePage)}}
		var entries []dirEntry
		c := call{method: http.MethodGet, path: base + "/repository/tree", query: q, repo: repo}
		if err := g.r.getJSON(ctx, c, &entries); err != nil {
			return nil, err
		}
		all = append(all, entries...)
		if len(entries) < glTreePage {
			break
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: %s in %s is not a directory or is empty", ErrNotFound,
			dir, repo)
	}
	return fileNames(all, "blob", dir, repo)
}

// fileNames keeps the entries of fileType, sorted; more than MaxDirEntries
// entries is ErrTooLarge.
func fileNames(entries []dirEntry, fileType, dir, repo string) ([]string, error) {
	if len(entries) > MaxDirEntries {
		return nil, fmt.Errorf("%w: %s in %s has more than %d entries", ErrTooLarge, dir,
			repo, MaxDirEntries)
	}
	names := []string{}
	for _, e := range entries {
		if e.Type == fileType && e.Name != "" {
			names = append(names, e.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}
