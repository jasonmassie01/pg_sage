package codehost

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// Reading a file at a commit and closing a pull request (cli-02b): an
// approval request opened as a pull request is checked at its merge
// commit, and closed by pg_sage when it was decided elsewhere or expired.

// validRef checks a branch name or commit sha before it is sent.
func validRef(ref string) error {
	if err := validBranch(ref); err != nil {
		return fmt.Errorf("%w: ref %q is not a branch or commit", ErrInvalid, ref)
	}
	return nil
}

// GetFileAt implements Host with the contents API on ref.
func (g *GitHub) GetFileAt(ctx context.Context, repo, path, ref string) (File, error) {
	if err := validRef(ref); err != nil {
		return File{}, err
	}
	return g.fileAt(ctx, repo, path, ref)
}

// ClosePR implements Host with PATCH /repos/{repo}/pulls/{n}.
func (g *GitHub) ClosePR(ctx context.Context, repo string, number int) error {
	base, err := ghRepo(repo)
	if err != nil {
		return err
	}
	if err := validNumber(number); err != nil {
		return err
	}
	return g.r.getJSON(ctx, call{method: http.MethodPatch,
		path: base + "/pulls/" + strconv.Itoa(number),
		body: map[string]string{"state": "closed"}, repo: repo, write: true}, nil)
}

// GetFileAt implements Host with the raw file endpoint on ref.
func (g *GitLab) GetFileAt(ctx context.Context, repo, path, ref string) (File, error) {
	if err := validRef(ref); err != nil {
		return File{}, err
	}
	return g.fileAt(ctx, repo, path, ref)
}

// ClosePR implements Host with PUT merge_requests/:iid state_event=close.
func (g *GitLab) ClosePR(ctx context.Context, repo string, number int) error {
	base, err := glProject(repo)
	if err != nil {
		return err
	}
	if err := validNumber(number); err != nil {
		return err
	}
	return g.r.getJSON(ctx, call{method: http.MethodPut,
		path: base + "/merge_requests/" + strconv.Itoa(number),
		body: map[string]string{"state_event": "close"}, repo: repo, write: true}, nil)
}

// merger is the login that merged the request, or "".
func (m glMR) merger() string {
	if m.MergedBy == nil {
		return ""
	}
	return m.MergedBy.Username
}

// mergeCommit is the commit the merge produced: the merge commit, or the
// squash commit of a squash merge (a fast-forward merge has neither).
func (m glMR) mergeCommit() string {
	if m.MergeSHA != nil && *m.MergeSHA != "" {
		return *m.MergeSHA
	}
	if m.SquashSHA != nil {
		return *m.SquashSHA
	}
	return ""
}
