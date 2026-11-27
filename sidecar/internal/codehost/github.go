package codehost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHubDefaultURL is the public GitHub API root.
const GitHubDefaultURL = "https://api.github.com"

// maxCommentBytes is GitHub's body limit, also used for GitLab.
const maxCommentBytes = 65536

// GitHub is the GitHub REST v3 client (github.com and GitHub Enterprise).
type GitHub struct{ r *rest }

var _ Host = (*GitHub)(nil)

// NewGitHub returns a GitHub client; opts.BaseURL empty means github.com.
func NewGitHub(opts Options) (*GitHub, error) {
	r, err := newRest("github", GitHubDefaultURL, opts, func(req *http.Request, tok string) {
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	})
	if err != nil {
		return nil, err
	}
	return &GitHub{r: r}, nil
}

// Name implements Host.
func (g *GitHub) Name() string { return g.r.name() }

// ghRepo checks repo is exactly owner/name and returns its escaped path.
func ghRepo(repo string) (string, error) {
	if err := ValidateRepo(repo); err != nil {
		return "", err
	}
	if strings.Count(repo, "/") != 1 {
		return "", fmt.Errorf("%w: GitHub repository %q must be owner/name", ErrInvalid, repo)
	}
	owner, name, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// escapePath escapes each segment of a validated file path.
func escapePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// SearchCode implements Host with GET /search/code.
func (g *GitHub) SearchCode(ctx context.Context, repo, query string) ([]SearchHit, error) {
	if _, err := ghRepo(repo); err != nil {
		return nil, err
	}
	if err := validQuery(query); err != nil {
		return nil, err
	}
	var out struct {
		Items []SearchHit `json:"items"`
	}
	q := url.Values{"q": {query + " repo:" + repo},
		"per_page": {strconv.Itoa(MaxSearchHits)}}
	c := call{method: http.MethodGet, path: "/search/code", query: q, repo: repo}
	if err := g.r.getJSON(ctx, c, &out); err != nil {
		return nil, err
	}
	return validHits(out.Items), nil
}

// validHits keeps at most MaxSearchHits hits with a valid relative path.
func validHits(items []SearchHit) []SearchHit {
	hits := make([]SearchHit, 0, len(items))
	for _, it := range items {
		if ValidatePath(it.Path) == nil && len(hits) < MaxSearchHits {
			hits = append(hits, it)
		}
	}
	return hits
}

type ghContent struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Size     int    `json:"size"`
	SHA      string `json:"sha"`
}

// GetFile implements Host with GET /repos/{repo}/contents/{path}.
func (g *GitHub) GetFile(ctx context.Context, repo, path string) (File, error) {
	return g.fileAt(ctx, repo, path, "")
}

// fileAt reads one file on ref ("" is the default branch).
func (g *GitHub) fileAt(ctx context.Context, repo, path, ref string) (File, error) {
	c, err := g.content(ctx, repo, path, ref)
	if err != nil {
		return File{}, err
	}
	if c.Type != "file" {
		return File{}, fmt.Errorf("%w: %s in %s is a %s, not a file", ErrNotFound, path,
			repo, c.Type)
	}
	if c.Size > MaxFileBytes || c.Encoding == "none" {
		return File{}, fmt.Errorf("%w: %s in %s is larger than %d bytes", ErrTooLarge,
			path, repo, MaxFileBytes)
	}
	if c.Encoding != "base64" {
		return File{}, fmt.Errorf("%w: %s in %s has unexpected encoding %q",
			ErrUnavailable, path, repo, c.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return File{}, fmt.Errorf("%w: %s in %s: content is not base64: %v",
			ErrUnavailable, path, repo, err)
	}
	return File{Path: path, Content: string(raw)}, nil
}

// content reads one contents entry, on ref or the default branch.
func (g *GitHub) content(ctx context.Context, repo, path, ref string) (ghContent, error) {
	base, err := ghRepo(repo)
	if err != nil {
		return ghContent{}, err
	}
	if err := ValidatePath(path); err != nil {
		return ghContent{}, err
	}
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	var raw json.RawMessage
	err = g.r.getJSON(ctx, call{method: http.MethodGet,
		path: base + "/contents/" + escapePath(path), query: q, repo: repo}, &raw)
	if err != nil {
		return ghContent{}, err
	}
	// A directory is answered with an array of its entries.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		return ghContent{Type: "dir"}, nil
	}
	var c ghContent
	if err := json.Unmarshal(raw, &c); err != nil {
		return ghContent{}, fmt.Errorf("%w: codehost %s: contents of %s in %s: "+
			"unexpected answer: %v", ErrUnavailable, g.Name(), path, repo, err)
	}
	return c, nil
}

type ghIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ListOpenPRs implements Host with GET /repos/{repo}/issues, which filters
// by label on the server; entries that are not pull requests are dropped.
func (g *GitHub) ListOpenPRs(ctx context.Context, repo, label string,
) ([]PullRequest, error) {
	base, err := ghRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := validLabel(label); err != nil {
		return nil, err
	}
	var issues []ghIssue
	q := url.Values{"state": {"open"}, "labels": {label}, "per_page": {"100"}}
	c := call{method: http.MethodGet, path: base + "/issues", query: q, repo: repo}
	if err := g.r.getJSON(ctx, c, &issues); err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(issues))
	for _, is := range issues {
		if is.PullRequest == nil {
			continue
		}
		pr := PullRequest{Number: is.Number, Title: is.Title, Body: is.Body, URL: is.HTMLURL}
		for _, l := range is.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		out = append(out, pr)
	}
	return out, nil
}

// Comment implements Host with POST /repos/{repo}/issues/{n}/comments.
func (g *GitHub) Comment(ctx context.Context, repo string, number int, body string) error {
	base, err := ghRepo(repo)
	if err != nil {
		return err
	}
	if err := validComment(number, body); err != nil {
		return err
	}
	return g.r.getJSON(ctx, call{method: http.MethodPost,
		path: base + "/issues/" + strconv.Itoa(number) + "/comments",
		body: map[string]string{"body": body}, repo: repo, write: true}, nil)
}

// validComment checks a comment before it is sent.
func validComment(number int, body string) error {
	if err := validNumber(number); err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" || len(body) > maxCommentBytes {
		return fmt.Errorf("%w: comment must be 1 to %d bytes", ErrInvalid, maxCommentBytes)
	}
	return nil
}

// PRState implements Host with GET /repos/{repo}/pulls/{n}.
func (g *GitHub) PRState(ctx context.Context, repo string, number int) (PRStatus, error) {
	base, err := ghRepo(repo)
	if err != nil {
		return PRStatus{}, err
	}
	if err := validNumber(number); err != nil {
		return PRStatus{}, err
	}
	var pr struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		Merged   bool       `json:"merged"`
		MergedAt *time.Time `json:"merged_at"`
		MergedBy *struct {
			Login string `json:"login"`
		} `json:"merged_by"`
		MergeSHA string `json:"merge_commit_sha"`
	}
	c := call{method: http.MethodGet, path: base + "/pulls/" + strconv.Itoa(number),
		repo: repo}
	if err := g.r.getJSON(ctx, c, &pr); err != nil {
		return PRStatus{}, err
	}
	st := PRStatus{Number: pr.Number, State: PROpen}
	switch {
	case pr.Merged || pr.MergedAt != nil:
		st.State, st.MergedAt, st.MergeSHA = PRMerged, pr.MergedAt, pr.MergeSHA
		if pr.MergedBy != nil {
			st.MergedBy = pr.MergedBy.Login
		}
	case pr.State == "closed":
		st.State = PRClosed
	case pr.State != "open":
		return PRStatus{}, fmt.Errorf("%w: pull request %s#%d has unknown state %q",
			ErrUnavailable, repo, number, pr.State)
	}
	return st, nil
}

// isNotFound reports a 404 from the host.
func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
