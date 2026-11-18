package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// `pg_sage_sidecar plan list|show|approve|reject|resume|cancel` (plan-03a)
// reads and decides plans through a running sidecar's API with an
// operator API token: SAGE_API_URL names the sidecar, SAGE_API_TOKEN (or
// SAGE_API_TOKEN_FILE) holds the token, which needs the read scope to read
// and the approve scope to decide. The token is sent only as a Bearer
// header, never printed, and only over https or to a loopback address.

const planSubcommand = "plan"

// Exit codes: decided (or read), refused by the sidecar (a stale hash, a
// plan not awaiting the decision, a server error), usage, auth.
const (
	planExitOK      = 0
	planExitRefused = 1
	planExitUsage   = 2
	planExitAuth    = 3
)

const planUsage = `usage: pg_sage_sidecar plan <command> [flags]
  list    [--database D] [--status S] [--json]
  show    <id> [--database D] [--json]
  approve <id> --hash H (--all | --up-to N | --step N) [--database D] [--json]
  reject  <id> --reason R [--database D] [--json]
  resume  <id> --hash H [--database D] [--json]
  cancel  <id> [--reason R] [--database D] [--json]
SAGE_API_URL is the sidecar's API (https, or http to localhost);
SAGE_API_TOKEN is an operator API token (read scope to read, approve to decide).`

// planArgs are one command's parsed flags.
type planArgs struct {
	id       int64
	database string
	status   string
	hash     string
	reason   string
	all      bool
	upTo     int
	step     int
	json     bool
}

// planClient is the sidecar API the command talks to (cli_client.go).
type planClient struct {
	api    *apiClient
	stdout io.Writer
	stderr io.Writer
}

// runPlanCommand runs `plan args...`; client nil uses a 30 s HTTP client.
func runPlanCommand(ctx context.Context, args []string, getenv func(string) string,
	client *http.Client, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, planUsage)
		return planExitUsage
	}
	cmd := args[0]
	a, err := parsePlanArgs(cmd, args[1:])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plan %s: %v\n%s\n", cmd, err, planUsage)
		return planExitUsage
	}
	c, code := newPlanClient(getenv, client, stdout, stderr)
	if c == nil {
		return code
	}
	return c.run(ctx, cmd, a)
}

func newPlanClient(getenv func(string) string, client *http.Client, stdout,
	stderr io.Writer) (*planClient, int) {
	base, err := planAPIURL(getenv("SAGE_API_URL"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plan: %v\n", err)
		return nil, planExitUsage
	}
	token := strings.TrimSpace(getenv("SAGE_API_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "plan: SAGE_API_TOKEN is not set: create an operator "+
			"API token (read scope to read, approve scope to decide)")
		return nil, planExitAuth
	}
	return &planClient{api: newAPIClient(base, token, client), stdout: stdout,
		stderr: stderr}, planExitOK
}

// planAPIURL parses SAGE_API_URL: https, or http only to a loopback host,
// so the token never crosses a network in clear.
func planAPIURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("SAGE_API_URL is not set: name the sidecar's API, " +
			"for example https://sage.example.com:8080")
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("SAGE_API_URL %q is not an http(s) URL with a host", raw)
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("SAGE_API_URL %q is plain http to another host: the token "+
			"would cross the network in clear; use https", raw)
	}
	return u, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parsePlanArgs reads `<id> [flags]` (list takes no id).
func parsePlanArgs(cmd string, args []string) (planArgs, error) {
	var a planArgs
	fs := flag.NewFlagSet("plan "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.database, "database", "", "the plan's database (fleet mode)")
	fs.BoolVar(&a.json, "json", false, "print the sidecar's JSON answer")
	switch cmd {
	case "list":
		fs.StringVar(&a.status, "status", "", "only plans in this status")
	case "approve":
		fs.StringVar(&a.hash, "hash", "", "the plan hash you reviewed")
		fs.BoolVar(&a.all, "all", false, "approve every step")
		fs.IntVar(&a.upTo, "up-to", 0, "approve steps 1..N")
		fs.IntVar(&a.step, "step", 0, "approve step N only")
	case "resume":
		fs.StringVar(&a.hash, "hash", "", "the plan hash you reviewed")
	case "reject", "cancel":
		fs.StringVar(&a.reason, "reason", "", "why")
	case "show":
	default:
		return a, fmt.Errorf("unknown command %q", cmd)
	}
	if cmd != "list" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return a, errors.New("the plan id comes first")
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return a, fmt.Errorf("plan id %q is not a positive integer", args[0])
		}
		a.id, args = id, args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	if fs.NArg() > 0 {
		return a, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return a, checkPlanArgs(cmd, a)
}

func checkPlanArgs(cmd string, a planArgs) error {
	switch cmd {
	case "approve":
		modes := 0
		for _, set := range []bool{a.all, a.upTo != 0, a.step != 0} {
			if set {
				modes++
			}
		}
		switch {
		case modes != 1:
			return errors.New("give exactly one of --all, --up-to N or --step N")
		case a.upTo < 0 || a.step < 0:
			return errors.New("--up-to and --step take a step number from 1")
		case strings.TrimSpace(a.hash) == "":
			return errors.New("--hash is required: approve the plan hash you reviewed")
		}
	case "resume":
		if strings.TrimSpace(a.hash) == "" {
			return errors.New("--hash is required: resume the plan hash you reviewed")
		}
	case "reject":
		if strings.TrimSpace(a.reason) == "" {
			return errors.New("--reason is required")
		}
	}
	return nil
}
