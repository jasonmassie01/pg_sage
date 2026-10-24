package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"
)

// pg_sage review submit|status|deployed: Change Review from CI or a
// terminal, over pg_sage's MCP Streamable HTTP endpoint with a read-scoped
// (propose for deployed), database-bound, expiring MCP token. The CLI sends
// the migration SQL and the diff, prints the cited review, and exits by its
// verdict. With --github it also keeps one marker comment on the pull
// request and sets the commit status 'pg_sage/db-review (<db>)' using the
// runner's own GITHUB_TOKEN; the GitHub token is never sent to pg_sage.

const reviewUsage = `usage: pg_sage review submit --url URL --database NAME
           (--sql-file FILE ... | --sql-dir DIR [--changed-only] | --diff-file FILE |
            --git-diff BASE...HEAD) [--pr-url URL] [--head-sha SHA]
           [--migration-tool TOOL] [--transactional=true|false]
           [--format markdown|json] [--out FILE] [--fail-on risky|review|never]
           [--wait[=DURATION]] [--github]
       pg_sage review status --url URL --database NAME (--id ID | --pr-url URL | --github)
       pg_sage review deployed --url URL --database NAME (--id ID | --pr-url URL)
           [--deployed-at RFC3339] [--commit SHA]
The MCP token comes from --token-file, PG_SAGE_TOKEN_FILE or PG_SAGE_TOKEN; the
URL from --url or PG_SAGE_URL. Exit codes: 0 pass, 1 verdict at or above
--fail-on (or not yet decided), 2 usage, 3 transport.`

// Exit codes.
const (
	reviewExitPass      = 0
	reviewExitFail      = 1
	reviewExitUsage     = 2
	reviewExitTransport = 3
)

const (
	reviewPollInterval = 5 * time.Second
	reviewDefaultWait  = 10 * time.Minute
	reviewHTTPTimeout  = 90 * time.Second
)

// reviewEnv is the CLI's environment; tests replace every part.
type reviewEnv struct {
	getenv         func(string) string
	stdout, stderr io.Writer
	http           *http.Client
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
	git            func(ctx context.Context, args ...string) ([]byte, error)
}

func defaultReviewEnv() reviewEnv {
	return reviewEnv{getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr,
		http: &http.Client{Timeout: reviewHTTPTimeout}, now: time.Now, sleep: sleepContext,
		git: func(ctx context.Context, args ...string) ([]byte, error) {
			return osexec.CommandContext(ctx, "git", args...).Output()
		}}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// usageError is a problem with the invocation (exit 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return usageError{msg: fmt.Sprintf(format, args...)}
}

// transportError is a failure to reach or talk to a server (exit 3).
type transportError struct{ msg string }

func (e transportError) Error() string { return e.msg }

// reviewToolError is a tools/call refused by pg_sage.
type reviewToolError struct {
	code int
	msg  string
}

func (e reviewToolError) Error() string {
	return fmt.Sprintf("pg_sage refused the call (%d): %s", e.code, e.msg)
}

// exitCode maps an error to the CLI's exit code: input pg_sage rejected
// (invalid arguments, unknown review or database) is usage; refused
// scopes and bindings, unavailability and transport failures are 3.
func exitCode(err error) int {
	var u usageError
	var tool reviewToolError
	switch {
	case err == nil:
		return reviewExitPass
	case errors.As(err, &u):
		return reviewExitUsage
	case errors.As(err, &tool):
		switch tool.code {
		case -32602, -32004, -32006, -32007:
			return reviewExitUsage
		}
	}
	return reviewExitTransport
}

// optionalBool is a --flag=true|false that may be absent.
type optionalBool struct{ v *bool }

func (b *optionalBool) String() string {
	if b == nil || b.v == nil {
		return ""
	}
	return strconv.FormatBool(*b.v)
}

func (b *optionalBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return errors.New("must be true or false")
	}
	b.v = &v
	return nil
}

func (b *optionalBool) IsBoolFlag() bool { return true }

// waitFlag is --wait (reviewDefaultWait) or --wait=DURATION.
type waitFlag struct{ d time.Duration }

func (w *waitFlag) String() string { return w.d.String() }

func (w *waitFlag) Set(s string) error {
	if s == "true" {
		w.d = reviewDefaultWait
		return nil
	}
	if s == "false" {
		w.d = 0
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || d > 6*time.Hour {
		return errors.New("must be a duration up to 6h, like 10m")
	}
	w.d = d
	return nil
}

func (w *waitFlag) IsBoolFlag() bool { return true }

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// runReviewCommand runs 'pg_sage review <args>' and returns the exit code.
func runReviewCommand(ctx context.Context, args []string, env reviewEnv) int {
	if len(args) == 0 {
		return reviewFail(env, usagef("a subcommand is required: submit, status or deployed"))
	}
	sub := args[0]
	switch sub {
	case "submit", "status", "deployed":
	default:
		return reviewFail(env, usagef("unknown subcommand %q", sub))
	}
	var o reviewOptions
	fs := reviewFlagSet(sub, &o)
	if err := fs.Parse(args[1:]); err != nil {
		return reviewFail(env, usagef("%v", err))
	}
	if fs.NArg() > 0 {
		return reviewFail(env, usagef("unexpected argument %q", fs.Arg(0)))
	}
	o.applyEnv(env.getenv)
	cli := &reviewCLI{env: env, o: o}
	code, err := cli.run(ctx, sub)
	if err != nil {
		return reviewFail(env, err)
	}
	return code
}

// reviewFail reports err (without any token) and returns its exit code.
func reviewFail(env reviewEnv, err error) int {
	code := exitCode(err)
	msg := "pg_sage review: " + err.Error()
	if code == reviewExitUsage {
		var u usageError
		if errors.As(err, &u) {
			msg += "\n" + reviewUsage
		}
	}
	_, _ = fmt.Fprintln(env.stderr, msg)
	return code
}

// runReviewSubcommand is the entry point from main.
func runReviewSubcommand(args []string) int {
	return runReviewCommand(context.Background(), args, defaultReviewEnv())
}

// newReviewFlagSet is a quiet flag set for sub.
func newReviewFlagSet(sub string) *flag.FlagSet {
	fs := flag.NewFlagSet("pg_sage review "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
