package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// GitHub mode (--github): the runner's own GITHUB_TOKEN keeps one comment
// per database on the pull request (found by its marker line and edited in
// place) and sets the commit status 'pg_sage/db-review (<db>)'. pg_sage
// holds no GitHub credentials, and this token is sent only to
// GITHUB_API_URL, never to pg_sage. Statuses and comments are written only
// when they change, so the 15-minute status job stays quiet.

const (
	defaultGitHubAPI = "https://api.github.com"
	maxGitHubBody    = 8 << 20
)

type githubPull struct {
	Number    int        `json:"number"`
	HTMLURL   string     `json:"html_url"`
	Title     string     `json:"title"`
	CreatedAt *time.Time `json:"created_at"`
	Head      struct {
		SHA  string `json:"sha"`
		Repo *struct {
			Fork bool `json:"fork"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}

type githubEvent struct {
	PullRequest *githubPull `json:"pull_request"`
}

type githubContext struct {
	api, repo, token string
	sha              string // GITHUB_SHA: the commit of a run without a pull request
	event            githubEvent
	http             *http.Client
	self             *string // the token's login once read ("": not a user token)
}

// loadGitHubContext reads the Actions environment; a missing variable or
// an unreadable event is a usage error naming it.
func loadGitHubContext(env reviewEnv) (*githubContext, error) {
	g := &githubContext{api: strings.TrimRight(env.getenv("GITHUB_API_URL"), "/"),
		repo: env.getenv("GITHUB_REPOSITORY"), token: env.getenv("GITHUB_TOKEN"),
		sha: env.getenv("GITHUB_SHA"), http: env.http}
	if g.api == "" {
		g.api = defaultGitHubAPI
	}
	for name, v := range map[string]string{"GITHUB_TOKEN": g.token,
		"GITHUB_REPOSITORY": g.repo, "GITHUB_EVENT_PATH": env.getenv("GITHUB_EVENT_PATH")} {
		if strings.TrimSpace(v) == "" {
			return nil, usagef("--github needs %s (set by GitHub Actions; pass the "+
				"runner's token as GITHUB_TOKEN)", name)
		}
	}
	if strings.Count(g.repo, "/") != 1 {
		return nil, usagef("GITHUB_REPOSITORY must be owner/repo, got %q", g.repo)
	}
	raw, err := os.ReadFile(env.getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return nil, usagef("cannot read GITHUB_EVENT_PATH: %v", errors.Unwrap(err))
	}
	if err := json.Unmarshal(raw, &g.event); err != nil {
		return nil, usagef("GITHUB_EVENT_PATH is not a GitHub event: %v", err)
	}
	return g, nil
}

// forkWithoutToken: a fork's pull request gets no repository secrets, so
// it has no pg_sage token; the review is skipped with a notice.
func (g *githubContext) forkWithoutToken(tokenFile string, getenv func(string) string) bool {
	pr := g.event.PullRequest
	if pr == nil || pr.Head.Repo == nil || !pr.Head.Repo.Fork {
		return false
	}
	_, err := reviewToken(tokenFile, getenv)
	return err != nil
}

// prInfo is the review request's pr object from the event.
func (g *githubContext) prInfo() map[string]any {
	pr := g.event.PullRequest
	if pr == nil {
		return nil
	}
	info := map[string]any{"url": pr.HTMLURL, "number": pr.Number, "title": pr.Title,
		"repo": g.repo, "head_sha": pr.Head.SHA}
	if pr.Base.SHA != "" {
		info["base_sha"] = pr.Base.SHA
	}
	if pr.CreatedAt != nil {
		info["opened_at"] = pr.CreatedAt.UTC().Format(time.RFC3339)
	}
	return info
}

// do sends one GitHub API request with the runner's token.
func (g *githubContext) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return transportError{msg: "cannot encode a GitHub request"}
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, body)
	if err != nil {
		return usagef("bad GITHUB_API_URL: %v", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return transportError{msg: strings.ReplaceAll(fmt.Sprintf("GitHub %s %s: %v",
			method, path, err), g.token, "[redacted]")}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxGitHubBody))
	if resp.StatusCode/100 != 2 {
		return transportError{msg: fmt.Sprintf("GitHub %s %s answered HTTP %d", method,
			path, resp.StatusCode)}
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return transportError{msg: fmt.Sprintf("GitHub %s %s answered invalid JSON",
			method, path)}
	}
	return nil
}

// publish writes the review comment and the commit status of the event's
// pull request; without one (workflow_dispatch) only the status of the
// reviewed commit (--head-sha, else GITHUB_SHA).
func (g *githubContext) publish(ctx context.Context, database string, r changereview.Review,
	gate reviewGate) error {
	pr := g.event.PullRequest
	sha := g.sha
	if pr != nil {
		if err := g.upsertComment(ctx, pr.Number, database, reviewMarkdown(r)); err != nil {
			return err
		}
		sha = pr.Head.SHA
	}
	if r.HeadSHA != "" {
		sha = r.HeadSHA
	}
	return g.setStatus(ctx, sha, database, gate)
}

// publishSkip marks a change that touches no database file as passing.
func (g *githubContext) publishSkip(ctx context.Context, database string) error {
	sha := g.sha
	if g.event.PullRequest != nil {
		sha = g.event.PullRequest.Head.SHA
	}
	if sha == "" {
		return nil
	}
	return g.setStatus(ctx, sha, database, reviewGate{
		state: "success", description: "no database-touching changes", pass: true})
}
