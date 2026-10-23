package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// maxReviewRequestBytes keeps a review_change request under the API's
// 1 MiB request-body limit (with room for the JSON-RPC envelope).
const maxReviewRequestBytes = 1<<20 - 8<<10

func (c *reviewCLI) submit(ctx context.Context) (int, error) {
	if c.gh != nil && c.gh.forkWithoutToken(c.o.tokenFile, c.env.getenv) {
		_, _ = fmt.Fprintln(c.env.stdout, "::notice::pg_sage review skipped: this fork "+
			"pull request has no pg_sage token")
		return reviewExitPass, nil
	}
	in, err := gatherReviewInput(ctx, c.o, c.env)
	if err != nil {
		return 0, err
	}
	if !in.touchesDatabase() {
		return c.skip(ctx)
	}
	req := c.request(in)
	if raw, err := json.Marshal(req); err == nil && len(raw) > maxReviewRequestBytes {
		return 0, usagef("the review request is %d bytes, over the %d-byte MCP request "+
			"limit; review fewer files (--sql-dir with --changed-only) or a smaller diff",
			len(raw), maxReviewRequestBytes)
	}
	if err := c.connect(ctx); err != nil {
		return 0, err
	}
	obj, err := c.client.call(ctx, "review_change", req)
	if err != nil {
		return 0, err
	}
	f, err := decodeFetched(obj)
	if err != nil {
		return 0, err
	}
	if f, err = c.await(ctx, f); err != nil {
		return 0, err
	}
	return c.finish(ctx, f)
}

// skip ends a submit whose change touches no database file.
func (c *reviewCLI) skip(ctx context.Context) (int, error) {
	_, _ = fmt.Fprintln(c.env.stdout, "pg_sage review: no database-touching changes")
	if c.gh != nil {
		if err := c.gh.publishSkip(ctx, c.o.database); err != nil {
			return 0, err
		}
	}
	return reviewExitPass, nil
}

// request is the review_change arguments: flags first, then the pull
// request of the GitHub event.
func (c *reviewCLI) request(in reviewInput) map[string]any {
	pr := map[string]any{}
	if c.gh != nil {
		for k, v := range c.gh.prInfo() {
			pr[k] = v
		}
	}
	if c.o.prURL != "" {
		pr["url"] = c.o.prURL
	}
	if c.o.headSHA != "" {
		pr["head_sha"] = c.o.headSHA
	}
	req := map[string]any{"database": c.o.database,
		"wait_seconds": changereview.MaxWaitSeconds}
	if len(in.files) > 0 {
		req["sql_files"] = in.files
	}
	if in.diff != "" {
		req["diff"] = in.diff
	}
	if len(pr) > 0 {
		req["pr"] = pr
	}
	if c.o.tool != "" {
		req["migration_tool"] = c.o.tool
	}
	if c.o.transactional.v != nil {
		req["transactional"] = *c.o.transactional.v
	}
	return req
}

func (c *reviewCLI) status(ctx context.Context) (int, error) {
	if c.o.id == "" && c.o.prURL == "" && (c.gh == nil || c.gh.event.PullRequest == nil) {
		if c.gh == nil {
			return 0, usagef("status needs --id, --pr-url or --github")
		}
		if err := c.connect(ctx); err != nil {
			return 0, err
		}
		return reviewExitPass, c.refreshOpenPulls(ctx)
	}
	if err := c.connect(ctx); err != nil {
		return 0, err
	}
	f, ok, err := c.currentReview(ctx)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, reviewToolError{code: -32004, msg: "no review of " + c.statusPRURL()}
	}
	if f, err = c.await(ctx, f); err != nil {
		return 0, err
	}
	return c.finish(ctx, f)
}

// statusPRURL is the PR whose review status reports: --pr-url, else the
// GitHub event's pull request.
func (c *reviewCLI) statusPRURL() string {
	if c.o.prURL == "" && c.gh != nil && c.gh.event.PullRequest != nil {
		return c.gh.event.PullRequest.HTMLURL
	}
	return c.o.prURL
}

// currentReview is the review --id names, else the newest review of the PR.
func (c *reviewCLI) currentReview(ctx context.Context) (fetched, bool, error) {
	if c.o.id != "" {
		f, err := c.getReview(ctx, c.o.id)
		return f, err == nil, err
	}
	return c.newestReview(ctx, c.statusPRURL())
}

// newestReview is the newest review of prURL, read in full.
func (c *reviewCLI) newestReview(ctx context.Context, prURL string) (fetched, bool, error) {
	obj, err := c.client.call(ctx, "list_change_reviews", map[string]any{
		"database": c.o.database, "pr_url": prURL, "limit": 1})
	if err != nil {
		return fetched{}, false, err
	}
	items, _ := obj["reviews"].([]any)
	if len(items) == 0 {
		return fetched{}, false, nil
	}
	first, _ := items[0].(map[string]any)
	brief, err := decodeFetched(first)
	if err != nil {
		return fetched{}, false, err
	}
	f, err := c.getReview(ctx, brief.review.ID)
	return f, err == nil, err
}

func (c *reviewCLI) deployed(ctx context.Context) (int, error) {
	if (c.o.id == "") == (c.o.prURL == "") {
		return 0, usagef("deployed needs exactly one of --id or --pr-url")
	}
	at := c.env.now().UTC()
	if c.o.deployedAt != "" {
		t, err := time.Parse(time.RFC3339, c.o.deployedAt)
		if err != nil {
			return 0, usagef("--deployed-at must be RFC 3339, like 2026-10-23T12:00:00Z")
		}
		at = t.UTC()
	}
	args := map[string]any{"database": c.o.database, "deployed_at": at.Format(time.RFC3339)}
	for k, v := range map[string]string{"id": c.o.id, "pr_url": c.o.prURL,
		"commit": c.o.commit} {
		if v != "" {
			args[k] = v
		}
	}
	if err := c.connect(ctx); err != nil {
		return 0, err
	}
	obj, err := c.client.call(ctx, "report_change_deployed", args)
	var tool reviewToolError
	if errors.As(err, &tool) && tool.code == -32009 {
		_, _ = fmt.Fprintf(c.env.stdout, "pg_sage review: already recorded: %s\n", tool.msg)
		return reviewExitPass, nil
	}
	if err != nil {
		return 0, err
	}
	f, err := decodeFetched(obj)
	if err != nil {
		return 0, err
	}
	r := f.review
	_, _ = fmt.Fprintf(c.env.stdout, "pg_sage review: review %s %s (%s) at %s\n", r.ID,
		r.LandingState, r.LandingSource, at.Format(time.RFC3339))
	return reviewExitPass, nil
}

// trimmed is s cut to n runes for a one-line description.
func trimmed(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
