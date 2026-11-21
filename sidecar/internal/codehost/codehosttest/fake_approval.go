package codehosttest

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Merge merges pull request number of repo as login: its files are written
// to the repository (each passed through edit when edit is not nil, as a
// person editing the PR before merging would) and the PR is merged at
// commit "merge-<number>". An unknown number is ignored.
func (f *Fake) Merge(repo string, number int, login string,
	edit func(path, content string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if number < 1 || number > len(f.opened) {
		return
	}
	if f.files[repo] == nil {
		f.files[repo] = map[string]string{}
	}
	for _, fc := range f.opened[number-1].Files {
		content := fc.Content
		if edit != nil {
			content = edit(fc.Path, content)
		}
		f.files[repo][fc.Path] = content
	}
	f.states[fmt.Sprintf("%s#%d", repo, number)] = codehost.PRStatus{Number: number,
		State: codehost.PRMerged, MergedBy: login, MergeSHA: fmt.Sprintf("merge-%d", number)}
}

// Closed returns the numbers ClosePR closed, in order.
func (f *Fake) Closed() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.closed...)
}

// GetFileAt implements codehost.Host; the fake keeps one version of each
// file, so ref is not used.
func (f *Fake) GetFileAt(ctx context.Context, repo, path, _ string) (codehost.File, error) {
	return f.GetFile(ctx, repo, path)
}

// ClosePR implements codehost.Host.
func (f *Fake) ClosePR(ctx context.Context, repo string, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx); err != nil {
		return err
	}
	key := fmt.Sprintf("%s#%d", repo, number)
	if _, ok := f.states[key]; !ok {
		return fmt.Errorf("%w: %s#%d", codehost.ErrNotFound, repo, number)
	}
	f.states[key] = codehost.PRStatus{Number: number, State: codehost.PRClosed}
	f.closed = append(f.closed, number)
	return nil
}
