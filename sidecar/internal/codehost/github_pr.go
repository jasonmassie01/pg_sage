package codehost

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
)

// OpenPR implements Host: it reads the base branch's head, creates
// req.Branch there, writes each file to it with the contents API and opens
// the pull request, then adds the labels.
func (g *GitHub) OpenPR(ctx context.Context, repo string, req PRRequest) (PullRequest, error) {
	base, err := ghRepo(repo)
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
	if err := g.createBranch(ctx, base, repo, req.Base, req.Branch); err != nil {
		return PullRequest{}, err
	}
	msg := req.CommitMessage
	if msg == "" {
		msg = req.Title
	}
	for _, f := range req.Files {
		if err := g.putFile(ctx, base, repo, req.Branch, msg, f); err != nil {
			return PullRequest{}, err
		}
	}
	pr, err := g.createPull(ctx, base, repo, req)
	if err != nil {
		return PullRequest{}, err
	}
	if len(req.Labels) > 0 {
		err = g.r.getJSON(ctx, call{method: http.MethodPost,
			path: base + "/issues/" + strconv.Itoa(pr.Number) + "/labels",
			body: map[string][]string{"labels": req.Labels}, repo: repo, write: true}, nil)
		if err != nil {
			return pr, fmt.Errorf("pull request %s#%d opened, labels not added: %w",
				repo, pr.Number, err)
		}
		pr.Labels = req.Labels
	}
	return pr, nil
}

func (g *GitHub) defaultBranch(ctx context.Context, base, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	c := call{method: http.MethodGet, path: base, repo: repo}
	if err := g.r.getJSON(ctx, c, &r); err != nil {
		return "", err
	}
	if err := validBranch(r.DefaultBranch); err != nil {
		return "", fmt.Errorf("%w: %s reports default branch %q", ErrUnavailable, repo,
			r.DefaultBranch)
	}
	return r.DefaultBranch, nil
}

// createBranch points refs/heads/branch at the head commit of from.
func (g *GitHub) createBranch(ctx context.Context, base, repo, from, branch string) error {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	c := call{method: http.MethodGet, path: base + "/git/ref/heads/" + escapePath(from),
		repo: repo}
	if err := g.r.getJSON(ctx, c, &ref); err != nil {
		return err
	}
	if ref.Object.SHA == "" {
		return fmt.Errorf("%w: branch %s of %s has no head commit", ErrUnavailable, from, repo)
	}
	return g.r.getJSON(ctx, call{method: http.MethodPost, path: base + "/git/refs",
		body: map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA},
		repo: repo, write: true}, nil)
}

// putFile creates or replaces one file on branch; replacing needs the
// current blob's sha.
func (g *GitHub) putFile(ctx context.Context, base, repo, branch, msg string,
	f FileChange) error {
	body := map[string]string{"message": msg, "branch": branch,
		"content": base64.StdEncoding.EncodeToString([]byte(f.Content))}
	cur, err := g.content(ctx, repo, f.Path, branch)
	switch {
	case err == nil:
		body["sha"] = cur.SHA
	case !isNotFound(err):
		return err
	}
	return g.r.getJSON(ctx, call{method: http.MethodPut,
		path: base + "/contents/" + escapePath(f.Path), body: body, repo: repo,
		write: true}, nil)
}

func (g *GitHub) createPull(ctx context.Context, base, repo string, req PRRequest,
) (PullRequest, error) {
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	err := g.r.getJSON(ctx, call{method: http.MethodPost, path: base + "/pulls",
		body: map[string]string{"title": req.Title, "body": req.Body, "head": req.Branch,
			"base": req.Base}, repo: repo, write: true}, &pr)
	if err != nil {
		return PullRequest{}, err
	}
	if pr.Number <= 0 {
		return PullRequest{}, fmt.Errorf("%w: %s answered a pull request without a number",
			ErrUnavailable, repo)
	}
	return PullRequest{Number: pr.Number, Title: req.Title, Body: req.Body,
		URL: pr.HTMLURL}, nil
}
