package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The marker comment and the commit status. A comment is pg_sage's when
// its first line is the marker of this database and a bot (the Actions
// token's identity) wrote it; a person's comment that copies the marker is
// never edited. The status job finds each pull request's review through
// pg_sage (by PR URL and head commit), never through a comment, so a
// planted marker cannot change a status.

const maxCommentPages = 10

type githubComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Type string `json:"type"`
	} `json:"user"`
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
			if comments[i].User.Type == "Bot" && strings.HasPrefix(comments[i].Body, prefix) {
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
		f, ok, err := c.newestReview(ctx, pull.HTMLURL)
		if err != nil {
			return err
		}
		if !ok || (f.review.HeadSHA != "" && f.review.HeadSHA != pull.Head.SHA) {
			continue // not reviewed yet, or reviewed at an older commit
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
