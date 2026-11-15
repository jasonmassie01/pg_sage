package codehost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitLabDefaultURL is the gitlab.com API root.
const GitLabDefaultURL = "https://gitlab.com/api/v4"

// GitLab is the GitLab REST v4 client (gitlab.com and self-managed).
type GitLab struct{ r *rest }

var _ Host = (*GitLab)(nil)

// NewGitLab returns a GitLab client; opts.BaseURL empty means gitlab.com.
func NewGitLab(opts Options) (*GitLab, error) {
	r, err := newRest("gitlab", GitLabDefaultURL, opts, func(req *http.Request, tok string) {
		req.Header.Set("PRIVATE-TOKEN", tok)
	})
	if err != nil {
		return nil, err
	}
	return &GitLab{r: r}, nil
}

// Name implements Host.
func (g *GitLab) Name() string { return g.r.name() }

// glProject checks repo and returns its escaped project path.
func glProject(repo string) (string, error) {
	if err := ValidateRepo(repo); err != nil {
		return "", err
	}
	return "/projects/" + url.PathEscape(repo), nil
}

// SearchCode implements Host with the project blob search.
func (g *GitLab) SearchCode(ctx context.Context, repo, query string) ([]SearchHit, error) {
	base, err := glProject(repo)
	if err != nil {
		return nil, err
	}
	if err := validQuery(query); err != nil {
		return nil, err
	}
	var blobs []SearchHit
	q := url.Values{"scope": {"blobs"}, "search": {query},
		"per_page": {strconv.Itoa(MaxSearchHits)}}
	c := call{method: http.MethodGet, path: base + "/search", query: q, repo: repo}
	if err := g.r.getJSON(ctx, c, &blobs); err != nil {
		return nil, err
	}
	// The blob search answers one entry per matching snippet; keep each file once.
	seen := map[string]bool{}
	unique := blobs[:0]
	for _, b := range blobs {
		if !seen[b.Path] {
			seen[b.Path] = true
			unique = append(unique, b)
		}
	}
	return validHits(unique), nil
}

// GetFile implements Host with the raw file endpoint on the default branch.
func (g *GitLab) GetFile(ctx context.Context, repo, path string) (File, error) {
	base, err := glProject(repo)
	if err != nil {
		return File{}, err
	}
	if err := ValidatePath(path); err != nil {
		return File{}, err
	}
	raw, err := g.r.do(ctx, call{method: http.MethodGet,
		path:  base + "/repository/files/" + url.PathEscape(path) + "/raw",
		query: url.Values{"ref": {"HEAD"}}, repo: repo})
	if err != nil {
		return File{}, err
	}
	if len(raw) > MaxFileBytes {
		return File{}, fmt.Errorf("%w: %s in %s is larger than %d bytes", ErrUnavailable,
			path, repo, MaxFileBytes)
	}
	return File{Path: path, Content: string(raw)}, nil
}

type glMR struct {
	IID         int        `json:"iid"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	WebURL      string     `json:"web_url"`
	State       string     `json:"state"`
	MergedAt    *time.Time `json:"merged_at"`
	Labels      []string   `json:"labels"`
}

func (m glMR) pullRequest() PullRequest {
	return PullRequest{Number: m.IID, Title: m.Title, Body: m.Description, URL: m.WebURL,
		Labels: m.Labels}
}

// ListOpenPRs implements Host with the project's opened merge requests.
func (g *GitLab) ListOpenPRs(ctx context.Context, repo, label string,
) ([]PullRequest, error) {
	base, err := glProject(repo)
	if err != nil {
		return nil, err
	}
	if err := validLabel(label); err != nil {
		return nil, err
	}
	var mrs []glMR
	q := url.Values{"state": {"opened"}, "labels": {label}, "per_page": {"100"}}
	c := call{method: http.MethodGet, path: base + "/merge_requests", query: q, repo: repo}
	if err := g.r.getJSON(ctx, c, &mrs); err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(mrs))
	for _, m := range mrs {
		out = append(out, m.pullRequest())
	}
	return out, nil
}

// Comment implements Host with a merge request note.
func (g *GitLab) Comment(ctx context.Context, repo string, number int, body string) error {
	base, err := glProject(repo)
	if err != nil {
		return err
	}
	if err := validComment(number, body); err != nil {
		return err
	}
	return g.r.getJSON(ctx, call{method: http.MethodPost,
		path: base + "/merge_requests/" + strconv.Itoa(number) + "/notes",
		body: map[string]string{"body": body}, repo: repo, write: true}, nil)
}

// PRState implements Host with GET merge_requests/:iid.
func (g *GitLab) PRState(ctx context.Context, repo string, number int) (PRStatus, error) {
	base, err := glProject(repo)
	if err != nil {
		return PRStatus{}, err
	}
	if err := validNumber(number); err != nil {
		return PRStatus{}, err
	}
	var m glMR
	c := call{method: http.MethodGet, path: base + "/merge_requests/" + strconv.Itoa(number),
		repo: repo}
	if err := g.r.getJSON(ctx, c, &m); err != nil {
		return PRStatus{}, err
	}
	st := PRStatus{Number: m.IID}
	switch m.State {
	case "opened":
		st.State = PROpen
	case "merged":
		st.State, st.MergedAt = PRMerged, m.MergedAt
	case "closed", "locked":
		st.State = PRClosed
	default:
		return PRStatus{}, fmt.Errorf("%w: merge request %s!%d has unknown state %q",
			ErrUnavailable, repo, number, m.State)
	}
	return st, nil
}

// OpenPR implements Host: one commit creating req.Branch from the base
// with every file, then the merge request with its labels.
func (g *GitLab) OpenPR(ctx context.Context, repo string, req PRRequest) (PullRequest, error) {
	base, err := glProject(repo)
	if err != nil {
		return PullRequest{}, err
	}
	if err := validPRRequest(req); err != nil {
		return PullRequest{}, err
	}
	if req.Base == "" {
		if req.Base, err = g.defaultBranch(ctx, base, repo); err != nil {
			return PullRequest{}, err
		}
	}
	if err := g.commit(ctx, base, repo, req); err != nil {
		return PullRequest{}, err
	}
	var m glMR
	err = g.r.getJSON(ctx, call{method: http.MethodPost, path: base + "/merge_requests",
		body: map[string]string{"source_branch": req.Branch, "target_branch": req.Base,
			"title": req.Title, "description": req.Body,
			"labels": strings.Join(req.Labels, ",")}, repo: repo, write: true}, &m)
	if err != nil {
		return PullRequest{}, err
	}
	if m.IID <= 0 {
		return PullRequest{}, fmt.Errorf("%w: %s answered a merge request without an iid",
			ErrUnavailable, repo)
	}
	return m.pullRequest(), nil
}

func (g *GitLab) defaultBranch(ctx context.Context, base, repo string) (string, error) {
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.r.getJSON(ctx, call{method: http.MethodGet, path: base, repo: repo},
		&p); err != nil {
		return "", err
	}
	if err := validBranch(p.DefaultBranch); err != nil {
		return "", fmt.Errorf("%w: %s reports default branch %q", ErrUnavailable, repo,
			p.DefaultBranch)
	}
	return p.DefaultBranch, nil
}

// commit creates req.Branch from req.Base with one commit of every file;
// a file that exists on the base is updated, any other is created.
func (g *GitLab) commit(ctx context.Context, base, repo string, req PRRequest) error {
	actions := make([]map[string]string, 0, len(req.Files))
	for _, f := range req.Files {
		action := "update"
		_, err := g.r.do(ctx, call{method: http.MethodHead,
			path:  base + "/repository/files/" + url.PathEscape(f.Path),
			query: url.Values{"ref": {req.Base}}, repo: repo})
		switch {
		case isNotFound(err):
			action = "create"
		case err != nil:
			return err
		}
		actions = append(actions, map[string]string{"action": action,
			"file_path": f.Path, "content": f.Content})
	}
	msg := req.CommitMessage
	if msg == "" {
		msg = req.Title
	}
	return g.r.getJSON(ctx, call{method: http.MethodPost, path: base + "/repository/commits",
		body: map[string]any{"branch": req.Branch, "start_branch": req.Base,
			"commit_message": msg, "actions": actions}, repo: repo, write: true}, nil)
}
