package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The marker comment and the commit status. A comment is pg_sage's when
// its first line is the marker of this database and the token's own
// account wrote it: a bot (the Actions token's identity) or the account
// GET /user names (a personal token); another person's comment that
// copies the marker is never edited. The status job finds each pull
// request's reviews through pg_sage, never through a comment, so a
// planted marker cannot change a status; and it gates on the reviews of
// the head commit, the most restrictive first, so a review someone else
// submits for the PR (any read-scoped caller can) can only hold the
// status back, never turn it green.

const maxCommentPages = 10

type githubComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

// login is the token's own account for a personal token, "" for the
// runner's token (GET /user is not open to an integration) or when it
// cannot be read: then only bot comments are pg_sage's.
func (g *githubContext) login(ctx context.Context) string {
	if g.self != nil {
		return *g.self
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := g.do(ctx, "GET", "/user", nil, &me); err != nil {
		me.Login = ""
	}
	g.self = &me.Login
	return me.Login
}

// ownComment reports whether c was written by the token's account.
func (g *githubContext) ownComment(ctx context.Context, c githubComment) bool {
	if c.User.Type == "Bot" {
		return true
	}
	me := g.login(ctx)
	return me != "" && c.User.Login == me
}

// markerPrefix is the start of line 1 of database's review comments.
func markerPrefix(database string) string {
	m := changereview.Marker(changereview.Review{Database: database})
	return m[:strings.Index(m, " id=")+1]
}

// statusContext is the commit status context of database.
func statusContext(database string) string { return "pg_sage/db-review (" + database + ")" }

func (g *githubContext) findComment(ctx context.Context, pr int, database string) (
	*githubComment, error) {
	prefix := markerPrefix(database)
	for page := 1; page <= maxCommentPages; page++ {
		var comments []githubComment
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", g.repo,
			pr, page)
		if err := g.do(ctx, "GET", path, nil, &comments); err != nil {
			return nil, err
		}
		for i := range comments {
			if strings.HasPrefix(comments[i].Body, prefix) && g.ownComment(ctx, comments[i]) {
				return &comments[i], nil
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	return nil, nil
}

// upsertComment creates database's comment on pr, or edits it when the
// body changed.
func (g *githubContext) upsertComment(ctx context.Context, pr int, database,
	body string) error {
	found, err := g.findComment(ctx, pr, database)
	if err != nil {
		return err
	}
	in := map[string]string{"body": body}
	if found == nil {
		return g.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", g.repo, pr),
			in, nil)
	}
	if found.Body == body {
		return nil
	}
	return g.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/comments/%d", g.repo, found.ID),
		in, nil)
}

// setStatus posts the gate as database's commit status on sha unless the
// newest status of that context already says the same.
func (g *githubContext) setStatus(ctx context.Context, sha, database string,
	gate reviewGate) error {
	if sha == "" {
		return usagef("--github needs the pull request's head commit (--head-sha)")
	}
	name := statusContext(database)
	desc := trimmed(gate.description, maxStatusDescription)
	var combined struct {
		Statuses []struct {
			State       string `json:"state"`
			Context     string `json:"context"`
			Description string `json:"description"`
		} `json:"statuses"`
	}
	sha = url.PathEscape(sha)
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/status?per_page=100",
		g.repo, sha), nil, &combined); err != nil {
		return err
	}
	for _, s := range combined.Statuses {
		if s.Context == name {
			if s.State == gate.state && s.Description == desc {
				return nil
			}
			break
		}
	}
	return g.do(ctx, "POST", fmt.Sprintf("/repos/%s/statuses/%s", g.repo, sha),
		map[string]string{"state": gate.state, "context": name, "description": desc}, nil)
}

// refreshOpenPulls is the scheduled status job: for each open pull request
// with a review of its head commit, refresh the comment and the status
// (a DBA verdict recorded since, a review that completed later).
func (c *reviewCLI) refreshOpenPulls(ctx context.Context) error {
	var pulls []githubPull
	if err := c.gh.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100",
		c.gh.repo), nil, &pulls); err != nil {
		return err
	}
	for _, pull := range pulls {
		f, ok, err := c.gatingReview(ctx, pull.HTMLURL, pull.Head.SHA)
		if err != nil {
			return err
		}
		if !ok {
			continue // no review of the head commit yet
		}
		if err := c.gh.upsertComment(ctx, pull.Number, c.o.database,
			reviewMarkdown(f.review)); err != nil {
			return err
		}
		gate := gateOf(f.review, c.o.failOn)
		if err := c.gh.setStatus(ctx, pull.Head.SHA, c.o.database, gate); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(c.env.stdout, "pg_sage review: #%d %s: %s\n", pull.Number,
			gate.state, gate.description)
	}
	return nil
}
