package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// The decision loop from a terminal or CI (cli-01a): `plan` with no verb
// answers "what needs a person now", `approve` approves one change (or a
// plan) bound to the card hash it printed, `apply` resumes a halted plan
// or says when an approved change runs, and `explain` prints a why-not
// chain. They talk to a running sidecar's API through cli_client.go with
// an operator API token (SAGE_API_TOKEN, or SAGE_API_TOKEN_FILE); the
// sidecar is SAGE_API_URL or --url. plan-03a's verbs (`plan list`, ...)
// are unchanged. None of them adds a write path: an approval goes through
// the sidecar's approval route, and so through the executor's policy
// gate, trust, verification, rollback and audit.

// Exit codes of the status form: 2 is "a person is needed", so a usage
// error is 64 and a CI gate never mistakes a typo for a pending approval.
const (
	statusExitClear       = 0
	statusExitError       = 1
	statusExitNeedsPerson = 2
	statusExitAuth        = 3
	statusExitUsage       = 64
)

// cliEnv is what a run reads and writes; client nil is a 30 s client.
type cliEnv struct {
	getenv func(string) string
	client *http.Client
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// cliCommands are the subcommands runCLI serves.
var cliCommands = map[string]bool{planSubcommand: true, "approve": true, "apply": true,
	"explain": true}

const cliUsage = `usage: pg_sage_sidecar <plan|approve|apply|explain> [flags]; --help for each`

// run is the process seam: the decision-loop subcommand in args[0] with
// the process environment (SAGE_API_TOKEN_FILE resolved) and stdin.
func run(args []string, stdout, stderr io.Writer) int {
	getenv, err := secretGetenv("SAGE_API_TOKEN")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", firstArg(args), err)
		return planExitAuth
	}
	return runCLI(context.Background(), args, cliEnv{getenv: getenv, stdin: os.Stdin,
		stdout: stdout, stderr: stderr})
}

// runCLI runs one decision-loop subcommand; args[0] names it.
func runCLI(ctx context.Context, args []string, env cliEnv) int {
	switch firstArg(args) {
	case planSubcommand:
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			return runPlanCommand(ctx, args[1:], env.getenv, env.client, env.stdout, env.stderr)
		}
		return runPlanStatus(ctx, args[1:], env)
	case "approve":
		return runApprove(ctx, args[1:], env)
	case "apply":
		return runApply(ctx, args[1:], env)
	case "explain":
		return runExplain(ctx, args[1:], env)
	}
	_, _ = fmt.Fprintf(env.stderr, "unknown command %q\n%s\n", firstArg(args), cliUsage)
	return planExitUsage
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// cliFlags are the flags every decision-loop command takes.
type cliFlags struct {
	url      string
	database string
	json     bool
}

func (f *cliFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.url, "url", "", "the sidecar's API (default $SAGE_API_URL)")
	fs.StringVar(&f.database, "database", "", "the database (fleet mode)")
	fs.BoolVar(&f.json, "json", false, "print stable JSON")
}

// newFlagSet is a quiet flag set: errors and help are printed by the
// command with its own usage.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseCLIFlags parses args; help prints usage to stdout. done reports a
// run that ends here with code.
func parseCLIFlags(fs *flag.FlagSet, args []string, usage string, usageCode int,
	env cliEnv) (code int, done bool) {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		_, _ = fmt.Fprintln(env.stdout, usage)
		return 0, true
	case err != nil:
		_, _ = fmt.Fprintf(env.stderr, "%s: %v\n%s\n", fs.Name(), err, usage)
		return usageCode, true
	}
	return 0, false
}

// usageError prints a usage error and returns code.
func usageError(env cliEnv, name, usage string, code int, format string, args ...any) int {
	_, _ = fmt.Fprintf(env.stderr, "%s: %s\n%s\n", name, fmt.Sprintf(format, args...), usage)
	return code
}

// positiveID reads a positive integer id.
func positiveID(raw, what string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%s %q is not a positive integer", what, raw)
	}
	return id, nil
}

// cliSession is one command's connection to the sidecar. human is where
// text for a person goes (the card, the prompt, progress): stdout, or
// stderr with --json, so that stdout holds only the one JSON document.
type cliSession struct {
	name  string
	api   *apiClient
	env   cliEnv
	human io.Writer
}

// connect resolves the sidecar URL (--url, else SAGE_API_URL) and the
// token; a missing URL exits 1, a missing token 3 (authCode).
func connect(name string, f cliFlags, env cliEnv, authCode int) (*cliSession, int) {
	raw := f.url
	if raw == "" {
		raw = env.getenv("SAGE_API_URL")
	}
	if strings.TrimSpace(raw) == "" {
		_, _ = fmt.Fprintf(env.stderr, "%s: set SAGE_API_URL or --url to the sidecar's API, "+
			"for example https://sage.example.com:8080\n", name)
		return nil, 1
	}
	base, err := planAPIURL(raw)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "%s: %v\n", name, err)
		return nil, 1
	}
	token := strings.TrimSpace(env.getenv("SAGE_API_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintf(env.stderr, "%s: SAGE_API_TOKEN is not set: create an operator "+
			"API token (read scope to read, approve scope to approve)\n", name)
		return nil, authCode
	}
	human := env.stdout
	if f.json {
		human = env.stderr
	}
	return &cliSession{name: name, api: newAPIClient(base, token, env.client), env: env,
		human: human}, 0
}

// printJSON prints raw, redacted and indented, as the command's stdout.
func (s *cliSession) printJSON(raw []byte) {
	_, _ = fmt.Fprintln(s.env.stdout, s.api.redact(indentJSON(raw)))
}

// fail prints err for a person and returns its exit code: authCode for a
// rejected or under-scoped token, 1 for everything else.
func (s *cliSession) fail(err error, authCode int) int {
	var refusal *cliRefusal
	if !errors.As(err, &refusal) {
		_, _ = fmt.Fprintf(s.env.stderr, "%s: %v\n", s.name, err)
		return 1
	}
	switch {
	case refusal.status == http.StatusUnauthorized:
		_, _ = fmt.Fprintf(s.env.stderr, "%s: token rejected (expired or revoked): %s\n",
			s.name, refusal.msg)
		return authCode
	case refusal.status == http.StatusConflict && refusal.code == "content_changed":
		_, _ = fmt.Fprintf(s.env.stderr, "%s: the change was edited after it was shown; "+
			"run plan again (%s)\n", s.name, refusal.code)
		return 1
	case refusal.status == http.StatusForbidden && refusal.code != "remote_approval_capped":
		_, _ = fmt.Fprintf(s.env.stderr, "%s: %s (%s)\n", s.name, refusal.msg,
			nonEmptyCode(refusal.code))
		return authCode
	}
	_, _ = fmt.Fprintf(s.env.stderr, "%s: %v\n", s.name, refusal)
	return 1
}

func nonEmptyCode(code string) string {
	if code == "" {
		return "forbidden"
	}
	return code
}

// confirm asks the person to type the database name unless yes; anything
// else (or no input) is a refusal.
func (s *cliSession) confirm(yes bool, database, what string) bool {
	if yes {
		return true
	}
	_, _ = fmt.Fprintf(s.human, "To %s, type the database name (%s): ", what, database)
	line := ""
	if s.env.stdin != nil {
		line, _ = bufio.NewReader(s.env.stdin).ReadString('\n')
	}
	_, _ = fmt.Fprintln(s.human)
	if strings.TrimSpace(line) == database && database != "" {
		return true
	}
	_, _ = fmt.Fprintf(s.env.stderr, "%s: not confirmed: nothing was %s; pass --yes or "+
		"type the database name\n", s.name, pastTense(what))
	return false
}

func pastTense(what string) string {
	if strings.HasPrefix(what, "resume") {
		return "resumed"
	}
	return "approved"
}
