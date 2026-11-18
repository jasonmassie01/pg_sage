package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// `pg_sage_sidecar pressure --database D [--table T] [--job J] [--batch N]
// [--json]` (guard-01b) asks a running sidecar's check_pressure route
// between batches and exits with the verdict, so a backfill job (a shell
// loop, a psql script, a CI step) can branch on it: 0 proceed, 10 slow,
// 20 stop. stdout is one line, "<verdict> batch=<n> factor=<f>
// sleep=<seconds>" (or the sidecar's JSON with --json); the reasons go to
// stderr. 2 is a usage error and 3 is no answer (the sidecar unreachable,
// a refused token, an unknown table, unreadable inputs), which a job must
// treat as stop. SAGE_API_URL names the sidecar and SAGE_API_TOKEN (or
// SAGE_API_TOKEN_FILE) holds a read-scope API token, sent only as a
// Bearer header, never printed, and only over https or to a loopback
// address.

const pressureSubcommand = "pressure"

const (
	pressureExitProceed  = 0
	pressureExitUsage    = 2
	pressureExitNoAnswer = 3
	pressureExitSlow     = 10
	pressureExitStop     = 20
)

const pressureUsage = `usage: pg_sage_sidecar pressure --database D [--table T] [--job J]
                              [--batch N] [--json]
exit 0 proceed, 10 slow (batch and sleep on stdout), 20 stop, 2 usage,
3 no answer (treat as stop).
SAGE_API_URL is the sidecar's API (https, or http to localhost);
SAGE_API_TOKEN is an API token with the read scope.`

// pressureArgs are the command's parsed flags.
type pressureArgs struct {
	database string
	table    string
	job      string
	batch    int
	json     bool
}

// pressureAnswer is the part of a verdict the command prints.
type pressureAnswer struct {
	Verdict        string   `json:"verdict"`
	BatchFactor    float64  `json:"batch_factor"`
	SuggestedBatch int      `json:"suggested_batch"`
	SleepSeconds   float64  `json:"sleep_seconds"`
	Reasons        []string `json:"reasons"`
}

var pressureExits = map[string]int{"proceed": pressureExitProceed,
	"slow": pressureExitSlow, "stop": pressureExitStop}

// runPressureCommand runs `pressure args...`; client nil uses a 20 s
// HTTP client (the route's own budget is 15 s).
func runPressureCommand(ctx context.Context, args []string, getenv func(string) string,
	client *http.Client, stdout, stderr io.Writer) int {
	a, err := parsePressureArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pressure: %v\n%s\n", err, pressureUsage)
		return pressureExitUsage
	}
	base, err := planAPIURL(getenv("SAGE_API_URL"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pressure: %v\n", err)
		return pressureExitUsage
	}
	token := strings.TrimSpace(getenv("SAGE_API_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "pressure: SAGE_API_TOKEN is not set: create an API "+
			"token with the read scope")
		return pressureExitUsage
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	raw, ans, err := askPressure(ctx, client, base, token, a)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pressure: %v; treat no answer as stop\n", err)
		return pressureExitNoAnswer
	}
	return printPressure(a, raw, ans, stdout, stderr)
}

// parsePressureArgs reads the flags; the docs test parses the documented
// commands with it.
func parsePressureArgs(args []string) (pressureArgs, error) {
	var a pressureArgs
	fs := flag.NewFlagSet(pressureSubcommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.database, "database", "", "the database the job writes")
	fs.StringVar(&a.table, "table", "", "the table the job writes (schema-qualified)")
	fs.StringVar(&a.job, "job", "", "the job's name (its application_name)")
	fs.IntVar(&a.batch, "batch", 0, "the job's current batch size")
	fs.BoolVar(&a.json, "json", false, "print the sidecar's JSON answer")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	switch {
	case fs.NArg() > 0:
		return a, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case strings.TrimSpace(a.database) == "":
		return a, errors.New("--database is required")
	case strings.ContainsAny(a.database, "/?#%"):
		return a, fmt.Errorf("--database %q is not a database name", a.database)
	case a.batch < 0:
		return a, errors.New("--batch is a whole number >= 0")
	}
	return a, nil
}

// askPressure GETs the verdict; any answer but a 200 with a known verdict
// is an error naming the sidecar's message or its address.
func askPressure(ctx context.Context, client *http.Client, base *url.URL, token string,
	a pressureArgs) ([]byte, pressureAnswer, error) {
	q := url.Values{}
	for k, v := range map[string]string{"table": a.table, "job": a.job} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if a.batch > 0 {
		q.Set("batch", strconv.Itoa(a.batch))
	}
	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/databases/" +
		url.PathEscape(a.database) + "/pressure"
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, pressureAnswer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, pressureAnswer{}, fmt.Errorf("the sidecar at %s is unreachable: %w",
			base.Host, errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, pressureAnswer{}, fmt.Errorf("reading the answer of %s: %w", base.Host, err)
	}
	return decodePressure(resp.StatusCode, base.Host, raw)
}

func decodePressure(status int, host string, raw []byte) ([]byte, pressureAnswer, error) {
	if status != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if body.Error == "" {
			body.Error = strings.TrimSpace(string(raw))
		}
		return nil, pressureAnswer{}, fmt.Errorf("%s answered HTTP %d: %s", host, status,
			body.Error)
	}
	var ans pressureAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, ans, fmt.Errorf("%s answered something that is not a verdict: %v", host,
			err)
	}
	if _, ok := pressureExits[ans.Verdict]; !ok {
		return nil, ans, fmt.Errorf("%s answered verdict %q, not proceed, slow or stop",
			host, ans.Verdict)
	}
	return raw, ans, nil
}

// printPressure prints the answer and returns its exit code.
func printPressure(a pressureArgs, raw []byte, ans pressureAnswer, stdout,
	stderr io.Writer) int {
	if a.json {
		_, _ = fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
	} else {
		_, _ = fmt.Fprintf(stdout, "%s batch=%d factor=%s sleep=%s\n", ans.Verdict,
			ans.SuggestedBatch, strconv.FormatFloat(ans.BatchFactor, 'f', -1, 64),
			strconv.FormatFloat(ans.SleepSeconds, 'f', -1, 64))
	}
	for _, r := range ans.Reasons {
		_, _ = fmt.Fprintf(stderr, "pressure: %s\n", r)
	}
	return pressureExits[ans.Verdict]
}
