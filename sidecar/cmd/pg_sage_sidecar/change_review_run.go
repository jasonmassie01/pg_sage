package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// reviewOptions are the flags of every review subcommand.
type reviewOptions struct {
	url, database, tokenFile string
	format, out, failOn      string
	github                   bool
	wait                     waitFlag

	sqlFiles             stringList
	sqlDir               string
	changedOnly          bool
	diffFile, gitDiff    string
	prURL, headSHA, tool string
	transactional        optionalBool

	id, deployedAt, commit string
}

// reviewFlagSet declares sub's flags into o.
func reviewFlagSet(sub string, o *reviewOptions) *flag.FlagSet {
	fs := newReviewFlagSet(sub)
	fs.StringVar(&o.url, "url", "", "pg_sage base URL (or PG_SAGE_URL)")
	fs.StringVar(&o.database, "database", "", "monitored database name")
	fs.StringVar(&o.tokenFile, "token-file", "", "file holding the MCP token")
	fs.StringVar(&o.prURL, "pr-url", "", "pull request URL")
	switch sub {
	case "submit":
		fs.Var(&o.sqlFiles, "sql-file", "migration SQL file (repeatable)")
		fs.StringVar(&o.sqlDir, "sql-dir", "", "directory of .sql migrations")
		fs.BoolVar(&o.changedOnly, "changed-only", false, "only --sql-dir files in the diff")
		fs.StringVar(&o.diffFile, "diff-file", "", "unified diff of the change")
		fs.StringVar(&o.gitDiff, "git-diff", "", "BASE...HEAD to diff with git")
		fs.StringVar(&o.headSHA, "head-sha", "", "head commit of the pull request")
		fs.StringVar(&o.tool, "migration-tool", "", "migration tool (flyway, rails, ...)")
		fs.Var(&o.transactional, "transactional", "the migration runs in one transaction")
	case "status":
		fs.StringVar(&o.id, "id", "", "review id")
	case "deployed":
		fs.StringVar(&o.id, "id", "", "review id")
		fs.StringVar(&o.deployedAt, "deployed-at", "", "deploy time, RFC 3339 (default now)")
		fs.StringVar(&o.commit, "commit", "", "deployed commit")
	}
	if sub != "deployed" {
		fs.StringVar(&o.format, "format", "markdown", "markdown or json")
		fs.StringVar(&o.out, "out", "", "write the review to this file")
		fs.StringVar(&o.failOn, "fail-on", "risky", "risky, review or never")
		fs.Var(&o.wait, "wait", "poll until the review completes (--wait=10m)")
		fs.BoolVar(&o.github, "github", false, "comment and set the commit status on GitHub")
	}
	return fs
}

// applyEnv fills the URL and token file from the environment.
func (o *reviewOptions) applyEnv(getenv func(string) string) {
	if o.url == "" {
		o.url = getenv("PG_SAGE_URL")
	}
	if o.tokenFile == "" {
		o.tokenFile = getenv("PG_SAGE_TOKEN_FILE")
	}
}

func (o *reviewOptions) validate(sub string) error {
	switch {
	case strings.TrimSpace(o.database) == "":
		return usagef("--database is required")
	case strings.TrimSpace(o.url) == "":
		return usagef("--url (or PG_SAGE_URL) is required")
	}
	if sub != "deployed" {
		if o.format != "markdown" && o.format != "json" {
			return usagef("--format must be markdown or json, got %q", o.format)
		}
		if _, ok := failOnRank[o.failOn]; !ok {
			return usagef("--fail-on must be risky, review or never, got %q", o.failOn)
		}
	}
	return nil
}

// reviewCLI is one invocation.
type reviewCLI struct {
	env    reviewEnv
	o      reviewOptions
	client *mcpClient
	gh     *githubContext
}

func (c *reviewCLI) run(ctx context.Context, sub string) (int, error) {
	if err := c.o.validate(sub); err != nil {
		return 0, err
	}
	if c.o.github {
		gh, err := loadGitHubContext(c.env)
		if err != nil {
			return 0, err
		}
		c.gh = gh
	}
	switch sub {
	case "submit":
		return c.submit(ctx)
	case "status":
		return c.status(ctx)
	}
	return c.deployed(ctx)
}

// connect resolves the token and opens the MCP session.
func (c *reviewCLI) connect(ctx context.Context) error {
	token, err := reviewToken(c.o.tokenFile, c.env.getenv)
	if err != nil {
		return err
	}
	client, err := newMCPClient(c.o.url, token, c.env.http)
	if err != nil {
		return err
	}
	if err := client.initialize(ctx); err != nil {
		return err
	}
	c.client = client
	return nil
}

// reviewToken reads the MCP token: the token file, else PG_SAGE_TOKEN.
func reviewToken(file string, getenv func(string) string) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", usagef("cannot read the MCP token file %s: %v", file,
				errors.Unwrap(err))
		}
		if token := strings.TrimSpace(string(raw)); token != "" {
			return token, nil
		}
		return "", usagef("the MCP token file %s is empty", file)
	}
	if token := strings.TrimSpace(getenv("PG_SAGE_TOKEN")); token != "" {
		return token, nil
	}
	return "", usagef("no MCP token: set --token-file or PG_SAGE_TOKEN_FILE (or " +
		"PG_SAGE_TOKEN) to a read-scoped token bound to the database")
}

// fetched is a review as a tool returned it: the typed review and the
// tool's JSON object (printed with --format json).
type fetched struct {
	review changereview.Review
	raw    map[string]any
}

func decodeFetched(obj map[string]any) (fetched, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return fetched{}, transportError{msg: "pg_sage returned a review that is not JSON"}
	}
	var r changereview.Review
	if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" {
		return fetched{}, transportError{msg: "pg_sage returned an unreadable review"}
	}
	return fetched{review: r, raw: obj}, nil
}

func (c *reviewCLI) getReview(ctx context.Context, id string) (fetched, error) {
	obj, err := c.client.call(ctx, "get_change_review", map[string]any{
		"database": c.o.database, "id": id})
	if err != nil {
		return fetched{}, err
	}
	return decodeFetched(obj)
}

// await polls get_change_review every reviewPollInterval while the review
// runs, for at most --wait.
func (c *reviewCLI) await(ctx context.Context, f fetched) (fetched, error) {
	for waited := time.Duration(0); f.review.State == changereview.StateRunning &&
		waited < c.o.wait.d; waited += reviewPollInterval {
		if err := c.env.sleep(ctx, reviewPollInterval); err != nil {
			return f, transportError{msg: "interrupted while waiting: " + err.Error()}
		}
		next, err := c.getReview(ctx, f.review.ID)
		if err != nil {
			return f, err
		}
		f = next
	}
	return f, nil
}

// finish prints the review, updates GitHub in --github mode and returns
// the gate's exit code.
func (c *reviewCLI) finish(ctx context.Context, f fetched) (int, error) {
	if err := c.print(f); err != nil {
		return 0, err
	}
	gate := gateOf(f.review, c.o.failOn)
	if f.review.State == changereview.StateRunning {
		_, _ = fmt.Fprintf(c.env.stderr, "pg_sage review: review %s is still running; "+
			"run 'pg_sage review status --id %s' or pass --wait\n", f.review.ID, f.review.ID)
	}
	if c.gh != nil {
		if err := c.gh.publish(ctx, c.o.database, f.review, gate); err != nil {
			return 0, err
		}
	}
	if gate.pass {
		return reviewExitPass, nil
	}
	return reviewExitFail, nil
}

// print writes the review as Markdown or JSON to stdout or --out.
func (c *reviewCLI) print(f fetched) error {
	var text string
	if c.o.format == "json" {
		raw, err := json.MarshalIndent(f.raw, "", "  ")
		if err != nil {
			return transportError{msg: "cannot encode the review"}
		}
		text = string(raw) + "\n"
	} else {
		text = reviewMarkdown(f.review)
	}
	if c.o.out == "" {
		_, err := fmt.Fprint(c.env.stdout, text)
		return err
	}
	if err := os.WriteFile(c.o.out, []byte(text), 0o644); err != nil {
		return usagef("cannot write --out %s: %v", c.o.out, errors.Unwrap(err))
	}
	return nil
}

// reviewMarkdown is the stored comment, or the running review's head.
func reviewMarkdown(r changereview.Review) string {
	md := r.Markdown
	if md == "" {
		md = changereview.RenderMarkdown(r, changereview.RenderOptions{})
	}
	if !strings.HasSuffix(md, "\n") {
		md += "\n"
	}
	return md
}
