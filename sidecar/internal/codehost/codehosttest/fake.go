// Package codehosttest is an in-memory codehost.Host for tests: files per
// repository, a substring code search, pull requests in a list, and a
// count of the calls that reached it.
package codehosttest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Fake is an in-memory code host. Its zero value is not usable; use New.
type Fake struct {
	mu     sync.Mutex
	files  map[string]map[string]string
	prs    map[string][]codehost.PullRequest
	states map[string]codehost.PRStatus
	err    error
	calls  int
	opened []codehost.PRRequest
	notes  []string
}

var _ codehost.Host = (*Fake)(nil)

// New returns an empty fake host.
func New() *Fake {
	return &Fake{files: map[string]map[string]string{},
		prs: map[string][]codehost.PullRequest{}, states: map[string]codehost.PRStatus{}}
}

// AddFile puts a file in repo.
func (f *Fake) AddFile(repo, path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files[repo] == nil {
		f.files[repo] = map[string]string{}
	}
	f.files[repo][path] = content
}

// Fail makes every later call return err (nil restores normal answers).
func (f *Fake) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Calls is how many calls reached the fake.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Opened returns the pull requests OpenPR received, in order.
func (f *Fake) Opened() []codehost.PRRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codehost.PRRequest(nil), f.opened...)
}

// Comments returns the comment bodies Comment received, in order.
func (f *Fake) Comments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...)
}

// SetState records the state PRState answers for repo#number.
func (f *Fake) SetState(repo string, st codehost.PRStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[fmt.Sprintf("%s#%d", repo, st.Number)] = st
}

// begin counts a call and returns the injected error.
func (f *Fake) begin(ctx context.Context) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.err
}

// Name implements codehost.Host.
func (f *Fake) Name() string { return "fake codehost" }

// SearchCode returns the files containing every space-separated term of
// query (double quotes ignored), sorted by path.
func (f *Fake) SearchCode(ctx context.Context, repo, query string,
) ([]codehost.SearchHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ReplaceAll(query, `"`, " "))
	var hits []codehost.SearchHit
	for path, content := range f.files[repo] {
		match := len(terms) > 0
		for _, t := range terms {
			match = match && strings.Contains(content, t)
		}
		if match {
			hits = append(hits, codehost.SearchHit{Path: path})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Path < hits[j].Path })
	if len(hits) > codehost.MaxSearchHits {
		hits = hits[:codehost.MaxSearchHits]
	}
	return hits, nil
}

// GetFile implements codehost.Host.
func (f *Fake) GetFile(ctx context.Context, repo, path string) (codehost.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return codehost.File{}, err
	}
	content, ok := f.files[repo][path]
	if !ok {
		return codehost.File{}, fmt.Errorf("%w: %s in %s", codehost.ErrNotFound, path, repo)
	}
	return codehost.File{Path: path, Content: content}, nil
}

// ListOpenPRs implements codehost.Host.
func (f *Fake) ListOpenPRs(ctx context.Context, repo, label string,
) ([]codehost.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	var out []codehost.PullRequest
	for _, pr := range f.prs[repo] {
		for _, l := range pr.Labels {
			if l == label {
				out = append(out, pr)
				break
			}
		}
	}
	return out, nil
}

// OpenPR implements codehost.Host: the files are written to the repo and
// the pull request is listed as open.
func (f *Fake) OpenPR(ctx context.Context, repo string, req codehost.PRRequest,
) (codehost.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return codehost.PullRequest{}, err
	}
	f.opened = append(f.opened, req)
	pr := codehost.PullRequest{Number: len(f.opened), Title: req.Title, Body: req.Body,
		URL:    fmt.Sprintf("https://fake.invalid/%s/pull/%d", repo, len(f.opened)),
		Labels: append([]string(nil), req.Labels...)}
	f.prs[repo] = append(f.prs[repo], pr)
	f.states[fmt.Sprintf("%s#%d", repo, pr.Number)] = codehost.PRStatus{Number: pr.Number,
		State: codehost.PROpen}
	return pr, nil
}

// Comment implements codehost.Host.
func (f *Fake) Comment(ctx context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return err
	}
	f.notes = append(f.notes, body)
	return nil
}

// PRState implements codehost.Host.
func (f *Fake) PRState(ctx context.Context, repo string, number int,
) (codehost.PRStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return codehost.PRStatus{}, err
	}
	st, ok := f.states[fmt.Sprintf("%s#%d", repo, number)]
	if !ok {
		return codehost.PRStatus{}, fmt.Errorf("%w: %s#%d", codehost.ErrNotFound, repo, number)
	}
	return st, nil
}
